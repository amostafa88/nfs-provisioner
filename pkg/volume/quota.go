/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package volume

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/docker/docker/pkg/mount"
	"github.com/golang/glog"
)

type quotaer interface {
	AddProject(string, string) (string, uint16, error)
	RemoveProject(string, uint16) error
	SetQuota(uint16, string, string) error
	UnsetQuota() error
}

type xfsQuotaer struct {
	xfsPath string

	// The file where we store mappings between project ids and directories, and
	// each project's quota limit information, for backup.
	// Similar to http://man7.org/linux/man-pages/man5/projects.5.html
	projectsFile string

	projectIds map[uint16]bool

	mapMutex  *sync.Mutex
	fileMutex *sync.Mutex
}

var _ quotaer = &xfsQuotaer{}

func newXfsQuotaer(xfsPath string) (*xfsQuotaer, error) {
	if _, err := os.Stat(xfsPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("xfs path %s does not exist!", xfsPath)
	}

	isXfs, err := isXfs(xfsPath)
	if err != nil {
		return nil, fmt.Errorf("error checking if xfs path %s is an XFS filesystem: %v", xfsPath, err)
	}
	if !isXfs {
		return nil, fmt.Errorf("xfs path %s is not an XFS filesystem", xfsPath)
	}

	entry, err := getMountEntry(path.Clean(xfsPath), "xfs")
	if err != nil {
		return nil, err
	}
	if !strings.Contains(entry.VfsOpts, "pquota") && !strings.Contains(entry.VfsOpts, "prjquota") {
		return nil, fmt.Errorf("xfs path %s was not mounted with pquota nor prjquota", xfsPath)
	}

	if _, err := exec.LookPath("xfs_quota"); err != nil {
		return nil, err
	}

	projectsFile := path.Join(xfsPath, "projects")
	projectIds := map[uint16]bool{}
	if _, err := os.Stat(projectsFile); os.IsNotExist(err) {
		file, err := os.Create(projectsFile)
		if err != nil {
			return nil, fmt.Errorf("error creating xfs projects file %s: %v", projectsFile, err)
		}
		file.Close()
	} else {
		re := regexp.MustCompile("(?m:^([0-9]+):/.+$)")
		projectIds, err = getExistingIds(projectsFile, re)
		if err != nil {
			glog.Errorf("error while populating projectIds map, there may be errors setting quotas later if projectIds are reused: %v", err)
		}
	}

	xfsQuotaer := &xfsQuotaer{
		xfsPath:      xfsPath,
		projectsFile: projectsFile,
		projectIds:   projectIds,
		mapMutex:     &sync.Mutex{},
		fileMutex:    &sync.Mutex{},
	}

	err = xfsQuotaer.restoreQuotas()
	if err != nil {
		return nil, fmt.Errorf("error restoring quotas from projects file %s: %v", projectsFile, err)
	}

	return xfsQuotaer, nil
}

func isXfs(xfsPath string) (bool, error) {
	cmd := exec.Command("stat", "-f", "-c", "%T", xfsPath)
	out, err := cmd.Output()
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(string(out)) != "xfs" {
		return false, nil
	}
	return true, nil
}

func getMountEntry(mountpoint, fstype string) (*mount.Info, error) {
	entries, err := mount.GetMounts()
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.Mountpoint == mountpoint && e.Fstype == fstype {
			return e, nil
		}
	}
	return nil, fmt.Errorf("mount entry for mountpoint %s, fstype %s not found", mountpoint, fstype)
}

func (q *xfsQuotaer) restoreQuotas() error {
	read, err := ioutil.ReadFile(q.projectsFile)
	if err != nil {
		return err
	}

	re := regexp.MustCompile("(?m:\n^([0-9]+):(.+):(.+)$\n)")

	matches := re.FindAllSubmatch(read, -1)
	for _, match := range matches {
		projectId, _ := strconv.ParseUint(string(match[1]), 10, 16)
		directory := string(match[2])
		bhard := string(match[3])

		// If directory referenced by projects file no longer exists, don't set a
		// quota for it: will fail
		if _, err := os.Stat(directory); os.IsNotExist(err) {
			q.RemoveProject(string(match[0]), uint16(projectId))
			continue
		}

		if err := q.SetQuota(uint16(projectId), directory, bhard); err != nil {
			return fmt.Errorf("error restoring quota for directory %s: %v", directory, err)
		}
	}

	return nil
}

func (q *xfsQuotaer) AddProject(directory, bhard string) (string, uint16, error) {
	projectId := generateId(q.mapMutex, q.projectIds)
	projectIdStr := strconv.FormatUint(uint64(projectId), 10)

	// Store project:directory mapping and also project's quota info
	block := "\n" + projectIdStr + ":" + directory + ":" + bhard + "\n"

	// Add the project block to the projects file
	if err := addToFile(q.fileMutex, q.projectsFile, block); err != nil {
		deleteId(q.mapMutex, q.projectIds, projectId)
		return "", 0, fmt.Errorf("error adding project block %s to projects file %s: %v", block, q.projectsFile, err)
	}

	// Specify the new project
	cmd := exec.Command("xfs_quota", "-x", "-c", fmt.Sprintf("project -s -p %s %s", directory, projectIdStr), q.xfsPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		deleteId(q.mapMutex, q.projectIds, projectId)
		removeFromFile(q.fileMutex, q.projectsFile, block)
		return "", 0, fmt.Errorf("xfs_quota failed with error: %v, output: %s", err, out)
	}

	return block, projectId, nil
}

func (q *xfsQuotaer) RemoveProject(block string, projectId uint16) error {
	deleteId(q.mapMutex, q.projectIds, projectId)
	return removeFromFile(q.fileMutex, q.projectsFile, block)
}

func (q *xfsQuotaer) SetQuota(projectId uint16, directory, bhard string) error {
	if !q.projectIds[projectId] {
		return fmt.Errorf("project with id %v has not been added", projectId)
	}
	projectIdStr := strconv.FormatUint(uint64(projectId), 10)

	cmd := exec.Command("xfs_quota", "-x", "-c", fmt.Sprintf("limit -p bhard=%s %s", bhard, projectIdStr), q.xfsPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("xfs_quota failed with error: %v, output: %s", err, out)
	}

	return nil
}

func (q *xfsQuotaer) UnsetQuota() error {
	return nil
}

type dummyQuotaer struct{}

var _ quotaer = &dummyQuotaer{}

func newDummyQuotaer() *dummyQuotaer {
	return &dummyQuotaer{}
}

func (q *dummyQuotaer) AddProject(_, _ string) (string, uint16, error) {
	return "", 0, nil
}
func (q *dummyQuotaer) RemoveProject(_ string, _ uint16) error {
	return nil
}
func (q *dummyQuotaer) SetQuota(_ uint16, _, _ string) error {
	return nil
}
func (q *dummyQuotaer) UnsetQuota() error {
	return nil
}
