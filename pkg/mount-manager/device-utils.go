/*
Copyright 2018 The Kubernetes Authors.

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

package mountmanager

import (
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog"
	"k8s.io/kubernetes/pkg/util/mount"
)

const (
	diskByIdPath         = "/dev/disk/by-id/"
	diskGooglePrefix     = "google-"
	diskScsiGooglePrefix = "scsi-0Google_PersistentDisk_"
	diskPartitionSuffix  = "-part"
	diskSDPath           = "/dev/sd"
	diskSDPattern        = "/dev/sd*"
	// How many times to retry for a consistent read of /proc/mounts.
	maxListTries = 3
	// Number of fields per line in /proc/mounts as per the fstab man page.
	expectedNumFieldsPerLine = 6
	// Location of the mount file to use
	procMountsPath = "/proc/mounts"
	// Location of the mountinfo file
	procMountInfoPath = "/proc/self/mountinfo"
	// 'fsck' found errors and corrected them
	fsckErrorsCorrected = 1
	// 'fsck' found errors but exited without correcting them
	fsckErrorsUncorrected = 4
	defaultMountCommand   = "mount"
)

// DeviceUtils are a collection of methods that act on the devices attached
// to a GCE Instance
type DeviceUtils interface {
	// GetDiskByIdPaths returns a list of all possible paths for a
	// given Persistent Disk
	GetDiskByIdPaths(deviceName string, partition string) []string

	// VerifyDevicePath returns the first of the list of device paths that
	// exists on the machine, or an empty string if none exists
	VerifyDevicePath(devicePaths []string) (string, error)

	// VerifyExistingMount tries to find a mount point on the machine from the
	// device at devicePath, which can be a symlink, to targetPath. It makes sure
	// that this mount point has the same ro/rw property and returns it or an error
	// if none exists.
	VerifyExistingMount(mounter *mount.SafeFormatAndMount, devicePath, targetPath string, readOnly bool) (*mount.MountPoint, error)
}

type deviceUtils struct {
}

var _ DeviceUtils = &deviceUtils{}

func NewDeviceUtils() *deviceUtils {
	return &deviceUtils{}
}

// Returns list of all /dev/disk/by-id/* paths for given PD.
func (m *deviceUtils) GetDiskByIdPaths(deviceName string, partition string) []string {
	devicePaths := []string{
		path.Join(diskByIdPath, diskGooglePrefix+deviceName),
		path.Join(diskByIdPath, diskScsiGooglePrefix+deviceName),
	}

	if partition != "" {
		for i, path := range devicePaths {
			devicePaths[i] = path + diskPartitionSuffix + partition
		}
	}

	return devicePaths
}

// Returns the first path that exists, or empty string if none exist.
func (m *deviceUtils) VerifyDevicePath(devicePaths []string) (string, error) {
	sdBefore, err := filepath.Glob(diskSDPattern)
	if err != nil {
		// Seeing this error means that the diskSDPattern is malformed.
		klog.Errorf("Error filepath.Glob(\"%s\"): %v\r\n", diskSDPattern, err)
	}
	sdBeforeSet := sets.NewString(sdBefore...)
	// TODO(#69): Verify udevadm works as intended in driver
	if err := udevadmChangeToNewDrives(sdBeforeSet); err != nil {
		// udevadm errors should not block disk detachment, log and continue
		klog.Errorf("udevadmChangeToNewDrives failed with: %v", err)
	}

	for _, path := range devicePaths {
		if pathExists, err := pathExists(path); err != nil {
			return "", fmt.Errorf("Error checking if path exists: %v", err)
		} else if pathExists {
			return path, nil
		}
	}

	return "", nil
}

/*
	Verifies that a fs mount refers to the same device as the one we expect with the correct permissions
	1) Target Path MUST be the vol referenced by vol ID
	2) Readonly MUST match
*/
func (m *deviceUtils) VerifyExistingMount(mounter *mount.SafeFormatAndMount, devicePath, targetPath string, readOnly bool) (*mount.MountPoint, error) {
	actualDevicePath, err := filepath.EvalSymlinks(devicePath)
	if err != nil {
		return nil, fmt.Errorf("error resolving device symlink: %v", devicePath)
	}
	allMountPoints, err := mounter.Interface.List()
	if err != nil {
		return nil, fmt.Errorf("error listing mounts: %v", err)
	}
	for _, mp := range allMountPoints {
		if mp.Device == actualDevicePath && mp.Path == targetPath {
			for _, opt := range mp.Opts {
				if readOnly && opt == "ro" {
					return &mp, nil
				} else if !readOnly && opt == "rw" {
					return &mp, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("could not verify mountpoint at %s for device %s", targetPath, devicePath)
}

// Triggers the application of udev rules by calling "udevadm trigger
// --action=change" for newly created "/dev/sd*" drives (exist only in
// after set). This is workaround for Issue #7972. Once the underlying
// issue has been resolved, this may be removed.
func udevadmChangeToNewDrives(sdBeforeSet sets.String) error {
	sdAfter, err := filepath.Glob(diskSDPattern)
	if err != nil {
		return fmt.Errorf("Error filepath.Glob(\"%s\"): %v\r\n", diskSDPattern, err)
	}

	for _, sd := range sdAfter {
		if !sdBeforeSet.Has(sd) {
			return udevadmChangeToDrive(sd)
		}
	}

	return nil
}

// Calls "udevadm trigger --action=change" on the specified drive.
// drivePath must be the block device path to trigger on, in the format "/dev/sd*", or a symlink to it.
// This is workaround for Issue #7972. Once the underlying issue has been resolved, this may be removed.
func udevadmChangeToDrive(drivePath string) error {
	klog.V(5).Infof("udevadmChangeToDrive: drive=%q", drivePath)

	// Evaluate symlink, if any
	drive, err := filepath.EvalSymlinks(drivePath)
	if err != nil {
		return fmt.Errorf("udevadmChangeToDrive: filepath.EvalSymlinks(%q) failed with %v.", drivePath, err)
	}
	klog.V(5).Infof("udevadmChangeToDrive: symlink path is %q", drive)

	// Check to make sure input is "/dev/sd*"
	if !strings.Contains(drive, diskSDPath) {
		return fmt.Errorf("udevadmChangeToDrive: expected input in the form \"%s\" but drive is %q.", diskSDPattern, drive)
	}

	// Call "udevadm trigger --action=change --property-match=DEVNAME=/dev/sd..."
	_, err = exec.Command(
		"udevadm",
		"trigger",
		"--action=change",
		fmt.Sprintf("--property-match=DEVNAME=%s", drive)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("udevadmChangeToDrive: udevadm trigger failed for drive %q with %v.", drive, err)
	}
	return nil
}

// PathExists returns true if the specified path exists.
func pathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	} else if os.IsNotExist(err) {
		return false, nil
	} else {
		return false, err
	}
}
