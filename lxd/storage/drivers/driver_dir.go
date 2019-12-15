package drivers

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/lxc/lxd/lxd/migration"
	"github.com/lxc/lxd/lxd/operations"
	"github.com/lxc/lxd/lxd/rsync"
	"github.com/lxc/lxd/lxd/storage/quota"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	"github.com/lxc/lxd/shared/ioprogress"
	log "github.com/lxc/lxd/shared/log15"
	"github.com/lxc/lxd/shared/units"
)

type dir struct {
	common
}

// Info returns info about the driver and its environment.
func (d *dir) Info() Info {
	return Info{
		Name:                  "dir",
		Version:               "1",
		OptimizedImages:       false,
		PreservesInodes:       false,
		Remote:                false,
		VolumeTypes:           []VolumeType{VolumeTypeCustom, VolumeTypeImage, VolumeTypeContainer, VolumeTypeVM},
		BlockBacking:          false,
		RunningQuotaResize:    true,
		RunningSnapshotFreeze: true,
	}
}

func (d *dir) Create() error {
	// WARNING: The Create() function cannot rely on any of the struct attributes being set.

	// Set default source if missing.
	if d.config["source"] == "" {
		d.config["source"] = GetPoolMountPath(d.name)
	}

	if !shared.PathExists(d.config["source"]) {
		return fmt.Errorf("Source path '%s' doesn't exist", d.config["source"])
	}

	// Check that if within LXD_DIR, we're at our expected spot.
	cleanSource := filepath.Clean(d.config["source"])
	if strings.HasPrefix(cleanSource, shared.VarPath()) && cleanSource != GetPoolMountPath(d.name) {
		return fmt.Errorf("Source path '%s' is within the LXD directory", d.config["source"])
	}

	// Check that the path is currently empty.
	isEmpty, err := shared.PathIsEmpty(d.config["source"])
	if err != nil {
		return err
	}

	if !isEmpty {
		return fmt.Errorf("Source path '%s' isn't empty", d.config["source"])
	}

	return nil
}

// Delete removes the storage pool from the storage device.
func (d *dir) Delete(op *operations.Operation) error {
	// On delete, wipe everything in the directory.
	err := wipeDirectory(GetPoolMountPath(d.name))
	if err != nil {
		return err
	}

	// Unmount the path.
	_, err = d.Unmount()
	if err != nil {
		return err
	}

	return nil
}

func (d *dir) Validate(config map[string]string) error {
	return nil
}

func (d *dir) Update(changedConfig map[string]string) error {
	return nil
}

// Mount mounts the storage pool.
func (d *dir) Mount() (bool, error) {
	path := GetPoolMountPath(d.name)

	// Check if we're dealing with an external mount.
	if d.config["source"] == path {
		return false, nil
	}

	// Check if already mounted.
	if sameMount(d.config["source"], path) {
		return false, nil
	}

	// Setup the bind-mount.
	err := tryMount(d.config["source"], path, "none", unix.MS_BIND, "")
	if err != nil {
		return false, err
	}

	return true, nil
}

// Unmount unmounts the storage pool.
func (d *dir) Unmount() (bool, error) {
	path := GetPoolMountPath(d.name)

	// Check if we're dealing with an external mount.
	if d.config["source"] == path {
		return false, nil
	}

	// Unmount until nothing is left mounted.
	return forceUnmount(path)
}

func (d *dir) GetResources() (*api.ResourcesStoragePool, error) {
	return d.vfsGetResources()
}

// GetVolumeUsage returns the disk space used by the volume.
func (d *dir) GetVolumeUsage(vol Volume) (int64, error) {
	volPath := vol.MountPath()
	ok, err := quota.Supported(volPath)
	if err != nil || !ok {
		return 0, nil
	}

	// Get the volume ID for the volume to access quota.
	volID, err := d.getVolID(vol.volType, vol.name)
	if err != nil {
		return -1, err
	}

	projectID := d.quotaProjectID(volID)

	// Get project quota used.
	size, err := quota.GetProjectUsage(volPath, projectID)
	if err != nil {
		return -1, err
	}

	return size, nil
}

// ValidateVolume validates the supplied volume config.
func (d *dir) ValidateVolume(vol Volume, removeUnknownKeys bool) error {
	return d.validateVolume(vol, nil, removeUnknownKeys)
}

// HasVolume indicates whether a specific volume exists on the storage pool.
func (d *dir) HasVolume(vol Volume) bool {
	if shared.PathExists(vol.MountPath()) {
		return true
	}

	return false
}

// GetVolumeDiskPath returns the location of a disk volume.
func (d *dir) GetVolumeDiskPath(vol Volume) (string, error) {
	return filepath.Join(vol.MountPath(), "root.img"), nil
}

// setupInitialQuota enables quota on a new volume and sets with an initial quota from config.
// Returns a revert function that can be used to remove the quota if there is a subsequent error.
func (d *dir) setupInitialQuota(vol Volume) (func(), error) {
	// Extract specified size from pool or volume config.
	size := d.config["volume.size"]
	if vol.config["size"] != "" {
		size = vol.config["size"]
	}

	volPath := vol.MountPath()

	// Get the volume ID for the new volume, which is used to set project quota.
	volID, err := d.getVolID(vol.volType, vol.name)
	if err != nil {
		return nil, err
	}

	// Define a function to revert the quota being setup.
	revertFunc := func() {
		d.deleteQuota(volPath, volID)
	}

	// Initialise the volume's quota using the volume ID.
	err = d.initQuota(volPath, volID)
	if err != nil {
		return nil, err
	}

	revert := true
	defer func() {
		if revert {
			revertFunc()
		}
	}()

	// Set the quota.
	err = d.setQuota(volPath, volID, size)
	if err != nil {
		return nil, err
	}

	revert = false
	return revertFunc, nil
}

// CreateVolume creates an empty volume and can optionally fill it by executing the supplied
// filler function.
func (d *dir) CreateVolume(vol Volume, filler *VolumeFiller, op *operations.Operation) error {
	volPath := vol.MountPath()
	err := vol.CreateMountPath()
	if err != nil {
		return err
	}

	revertPath := true
	defer func() {
		if revertPath {
			os.RemoveAll(volPath)
		}
	}()

	// Create sparse loopback file if volume is block.
	rootBlockPath := ""
	if vol.contentType == ContentTypeBlock {
		// We expect the filler to copy the VM image into this path.
		rootBlockPath, err = d.GetVolumeDiskPath(vol)
		if err != nil {
			return err
		}
	} else {
		revertFunc, err := d.setupInitialQuota(vol)
		if err != nil {
			return err
		}

		if revertFunc != nil {
			defer func() {
				if revertPath {
					revertFunc()
				}
			}()
		}
	}

	// Run the volume filler function if supplied.
	if filler != nil && filler.Fill != nil {
		d.logger.Debug("Running filler function")
		err = filler.Fill(volPath, rootBlockPath)
		if err != nil {
			return err
		}
	}

	// If we are creating a block volume, resize it to the requested size or 10GB.
	// We expect the filler function to have converted the qcow2 image to raw into the rootBlockPath.
	if vol.contentType == ContentTypeBlock {
		// Extract specified size from pool or volume config.
		blockSize := d.config["volume.size"]
		if vol.config["size"] != "" {
			blockSize = vol.config["size"]
		}

		if blockSize == "" {
			blockSize = "10GB"
		}

		blockSizeBytes, err := units.ParseByteSizeString(blockSize)
		if err != nil {
			return err
		}

		if shared.PathExists(rootBlockPath) {
			_, err = shared.RunCommand("qemu-img", "resize", "-f", "raw", rootBlockPath, fmt.Sprintf("%d", blockSizeBytes))
			if err != nil {
				return fmt.Errorf("Failed resizing disk image %s to size %s: %v", rootBlockPath, blockSize, err)
			}
		} else {
			// If rootBlockPath doesn't exist, then there has been no filler function
			// supplied to create it from another source. So instead create an empty
			// volume (use for PXE booting a VM).
			_, err = shared.RunCommand("qemu-img", "create", "-f", "raw", rootBlockPath, fmt.Sprintf("%d", blockSizeBytes))
			if err != nil {
				return fmt.Errorf("Failed creating disk image %s as size %s: %v", rootBlockPath, blockSize, err)
			}
		}
	}

	revertPath = false
	return nil
}

// MigrateVolume sends a volume for migration.
func (d *dir) MigrateVolume(vol Volume, conn io.ReadWriteCloser, volSrcArgs migration.VolumeSourceArgs, op *operations.Operation) error {
	if vol.contentType != ContentTypeFS {
		return fmt.Errorf("Content type not supported")
	}

	if volSrcArgs.MigrationType.FSType != migration.MigrationFSType_RSYNC {
		return fmt.Errorf("Migration type not supported")
	}

	bwlimit := d.config["rsync.bwlimit"]

	for _, snapName := range volSrcArgs.Snapshots {
		snapshot, err := vol.NewSnapshot(snapName)
		if err != nil {
			return err
		}

		// Send snapshot to recipient (ensure local snapshot volume is mounted if needed).
		err = snapshot.MountTask(func(mountPath string, op *operations.Operation) error {
			var wrapper *ioprogress.ProgressTracker
			if volSrcArgs.TrackProgress {
				wrapper = migration.ProgressTracker(op, "fs_progress", snapshot.name)
			}

			path := shared.AddSlash(mountPath)
			return rsync.Send(snapshot.name, path, conn, wrapper, volSrcArgs.MigrationType.Features, bwlimit, d.state.OS.ExecPath)
		}, op)
		if err != nil {
			return err
		}
	}

	// Send volume to recipient (ensure local volume is mounted if needed).
	return vol.MountTask(func(mountPath string, op *operations.Operation) error {
		var wrapper *ioprogress.ProgressTracker
		if volSrcArgs.TrackProgress {
			wrapper = migration.ProgressTracker(op, "fs_progress", vol.name)
		}

		path := shared.AddSlash(mountPath)
		return rsync.Send(vol.name, path, conn, wrapper, volSrcArgs.MigrationType.Features, bwlimit, d.state.OS.ExecPath)
	}, op)
}

// CreateVolumeFromMigration creates a volume being sent via a migration.
func (d *dir) CreateVolumeFromMigration(vol Volume, conn io.ReadWriteCloser, volTargetArgs migration.VolumeTargetArgs, preFiller *VolumeFiller, op *operations.Operation) error {
	if vol.contentType != ContentTypeFS {
		return fmt.Errorf("Content type not supported")
	}

	if volTargetArgs.MigrationType.FSType != migration.MigrationFSType_RSYNC {
		return fmt.Errorf("Migration type not supported")
	}

	// Get the volume ID for the new volumes, which is used to set project quota.
	volID, err := d.getVolID(vol.volType, vol.name)
	if err != nil {
		return err
	}

	// Create the main volume path.
	volPath := vol.MountPath()
	err = vol.CreateMountPath()
	if err != nil {
		return err
	}

	// Create slice of snapshots created if revert needed later.
	revertSnaps := []string{}
	defer func() {
		if revertSnaps == nil {
			return
		}

		// Remove any paths created if we are reverting.
		for _, snapName := range revertSnaps {
			fullSnapName := GetSnapshotVolumeName(vol.name, snapName)
			snapVol := NewVolume(d, d.name, vol.volType, vol.contentType, fullSnapName, vol.config)
			d.DeleteVolumeSnapshot(snapVol, op)
		}

		os.RemoveAll(volPath)
	}()

	// Ensure the volume is mounted.
	err = vol.MountTask(func(mountPath string, op *operations.Operation) error {
		path := shared.AddSlash(mountPath)

		// Run the volume pre-filler function if supplied.
		if preFiller != nil && preFiller.Fill != nil {
			d.logger.Debug("Running pre-filler function", log.Ctx{"volume": vol.name, "path": path})
			err = preFiller.Fill(path, "")
			if err != nil {
				return err
			}
			d.logger.Debug("Finished pre-filler function", log.Ctx{"volume": vol.name})
		}

		// Snapshots are sent first by the sender, so create these first.
		for _, snapName := range volTargetArgs.Snapshots {
			// Receive the snapshot
			var wrapper *ioprogress.ProgressTracker
			if volTargetArgs.TrackProgress {
				wrapper = migration.ProgressTracker(op, "fs_progress", snapName)
			}

			d.logger.Debug("Receiving volume", log.Ctx{"volume": vol.name, "snapshot": snapName, "path": path})
			err = rsync.Recv(path, conn, wrapper, volTargetArgs.MigrationType.Features)
			if err != nil {
				return err
			}

			// Create the snapshot itself.
			fullSnapshotName := GetSnapshotVolumeName(vol.name, snapName)
			snapshotVol := NewVolume(d, d.name, vol.volType, vol.contentType, fullSnapshotName, vol.config)

			err = d.CreateVolumeSnapshot(snapshotVol, op)
			if err != nil {
				return err
			}

			// Setup the revert.
			revertSnaps = append(revertSnaps, snapName)
		}

		// Initialise the volume's quota using the volume ID.
		err = d.initQuota(volPath, volID)
		if err != nil {
			return err
		}

		// Set the quota if specified in volConfig or pool config.
		err = d.setQuota(volPath, volID, vol.config["size"])
		if err != nil {
			return err
		}

		// Receive the main volume from sender.
		var wrapper *ioprogress.ProgressTracker
		if volTargetArgs.TrackProgress {
			wrapper = migration.ProgressTracker(op, "fs_progress", vol.name)
		}

		d.logger.Debug("Receiving volume", log.Ctx{"volume": vol.name, "path": path})
		err = rsync.Recv(path, conn, wrapper, volTargetArgs.MigrationType.Features)
		if err != nil {
			return err
		}

		// Receive the final main volume sync if needed.
		if volTargetArgs.Live {
			if volTargetArgs.TrackProgress {
				wrapper = migration.ProgressTracker(op, "fs_progress", vol.name)
			}

			d.logger.Debug("Receiving volume (final stage)", log.Ctx{"vol": vol.name, "path": path})
			err = rsync.Recv(path, conn, wrapper, volTargetArgs.MigrationType.Features)
			if err != nil {
				return err
			}
		}

		return nil
	}, op)
	if err != nil {
		return err
	}

	revertSnaps = nil
	return nil
}

// CreateVolumeFromCopy provides same-pool volume copying functionality.
func (d *dir) CreateVolumeFromCopy(vol Volume, srcVol Volume, copySnapshots bool, op *operations.Operation) error {
	var err error
	var srcSnapshots []Volume

	if copySnapshots && !srcVol.IsSnapshot() {
		// Get the list of snapshots from the source.
		srcSnapshots, err = srcVol.Snapshots(op)
		if err != nil {
			return err
		}
	}

	return d.copyVolume(vol, srcVol, srcSnapshots, op)
}

// RefreshVolume provides same-pool volume and specific snapshots syncing functionality.
func (d *dir) RefreshVolume(vol Volume, srcVol Volume, srcSnapshots []Volume, op *operations.Operation) error {
	return d.copyVolume(vol, srcVol, srcSnapshots, op)
}

// copyVolume copies a volume and its specific snapshots.
func (d *dir) copyVolume(vol Volume, srcVol Volume, srcSnapshots []Volume, op *operations.Operation) error {
	if vol.contentType != ContentTypeFS || srcVol.contentType != ContentTypeFS {
		return fmt.Errorf("Content type not supported")
	}

	bwlimit := d.config["rsync.bwlimit"]

	// Get the volume ID for the new volumes, which is used to set project quota.
	volID, err := d.getVolID(vol.volType, vol.name)
	if err != nil {
		return err
	}

	// Create the main volume path.
	volPath := vol.MountPath()
	err = vol.CreateMountPath()
	if err != nil {
		return err
	}

	// Create slice of snapshots created if revert needed later.
	revertSnaps := []string{}
	defer func() {
		if revertSnaps == nil {
			return
		}

		// Remove any paths created if we are reverting.
		for _, snapName := range revertSnaps {
			fullSnapName := GetSnapshotVolumeName(vol.name, snapName)
			snapVol := NewVolume(d, d.name, vol.volType, vol.contentType, fullSnapName, vol.config)
			d.DeleteVolumeSnapshot(snapVol, op)
		}

		os.RemoveAll(volPath)
	}()

	// Ensure the volume is mounted.
	err = vol.MountTask(func(mountPath string, op *operations.Operation) error {
		// If copying snapshots is indicated, check the source isn't itself a snapshot.
		if len(srcSnapshots) > 0 && !srcVol.IsSnapshot() {
			for _, srcSnapshot := range srcSnapshots {
				_, snapName, _ := shared.InstanceGetParentAndSnapshotName(srcSnapshot.name)

				// Mount the source snapshot.
				err = srcSnapshot.MountTask(func(srcMountPath string, op *operations.Operation) error {
					// Copy the snapshot.
					_, err = rsync.LocalCopy(srcMountPath, mountPath, bwlimit, true)
					return err
				}, op)

				fullSnapName := GetSnapshotVolumeName(vol.name, snapName)
				snapVol := NewVolume(d, d.name, vol.volType, vol.contentType, fullSnapName, vol.config)

				// Create the snapshot itself.
				err = d.CreateVolumeSnapshot(snapVol, op)
				if err != nil {
					return err
				}

				// Setup the revert.
				revertSnaps = append(revertSnaps, snapName)
			}
		}

		// Initialise the volume's quota using the volume ID.
		err = d.initQuota(volPath, volID)
		if err != nil {
			return err
		}

		// Set the quota if specified in volConfig or pool config.
		err = d.setQuota(volPath, volID, vol.config["size"])
		if err != nil {
			return err
		}

		// Copy source to destination (mounting each volume if needed).
		return srcVol.MountTask(func(srcMountPath string, op *operations.Operation) error {
			_, err := rsync.LocalCopy(srcMountPath, mountPath, bwlimit, true)
			return err
		}, op)
	}, op)
	if err != nil {
		return err
	}

	revertSnaps = nil // Don't revert.
	return nil
}

// VolumeSnapshots returns a list of snapshots for the volume.
func (d *dir) VolumeSnapshots(vol Volume, op *operations.Operation) ([]string, error) {
	return d.vfsVolumeSnapshots(vol, op)
}

// UpdateVolume applies config changes to the volume.
func (d *dir) UpdateVolume(vol Volume, changedConfig map[string]string) error {
	if vol.contentType != ContentTypeFS {
		return fmt.Errorf("Content type not supported")
	}

	if _, changed := changedConfig["size"]; changed {
		volID, err := d.getVolID(vol.volType, vol.name)
		if err != nil {
			return err
		}

		// Set the quota if specified in volConfig or pool config.
		err = d.setQuota(vol.MountPath(), volID, changedConfig["size"])
		if err != nil {
			return err
		}
	}

	return nil
}

// RenameVolume renames a volume and its snapshots.
func (d *dir) RenameVolume(vol Volume, newVolName string, op *operations.Operation) error {
	return d.vfsRenameVolume(vol, newVolName, op)
}

// RestoreVolume restores a volume from a snapshot.
func (d *dir) RestoreVolume(vol Volume, snapshotName string, op *operations.Operation) error {
	srcPath := GetVolumeMountPath(d.name, vol.volType, GetSnapshotVolumeName(vol.name, snapshotName))
	if !shared.PathExists(srcPath) {
		return fmt.Errorf("Snapshot not found")
	}

	volPath := vol.MountPath()

	// Restore using rsync.
	bwlimit := d.config["rsync.bwlimit"]
	_, err := rsync.LocalCopy(srcPath, volPath, bwlimit, true)
	if err != nil {
		return fmt.Errorf("Failed to rsync volume: %s", err)
	}

	return nil
}

// DeleteVolume deletes a volume of the storage device. If any snapshots of the volume remain then
// this function will return an error.
func (d *dir) DeleteVolume(vol Volume, op *operations.Operation) error {
	snapshots, err := d.VolumeSnapshots(vol, op)
	if err != nil {
		return err
	}

	if len(snapshots) > 0 {
		return fmt.Errorf("Cannot remove a volume that has snapshots")
	}

	volPath := vol.MountPath()

	// If the volume doesn't exist, then nothing more to do.
	if !shared.PathExists(volPath) {
		return nil
	}

	// Get the volume ID for the volume, which is used to remove project quota.
	volID, err := d.getVolID(vol.volType, vol.name)
	if err != nil {
		return err
	}

	// Remove the project quota.
	err = d.deleteQuota(volPath, volID)
	if err != nil {
		return err
	}

	// Remove the volume from the storage device.
	err = os.RemoveAll(volPath)
	if err != nil {
		return err
	}

	// Although the volume snapshot directory should already be removed, lets remove it here
	// to just in case the top-level directory is left.
	err = deleteParentSnapshotDirIfEmpty(d.name, vol.volType, vol.name)
	if err != nil {
		return err
	}

	return nil
}

// MountVolume simulates mounting a volume. As dir driver doesn't have volumes to mount it returns
// false indicating that there is no need to issue an unmount.
func (d *dir) MountVolume(vol Volume, op *operations.Operation) (bool, error) {
	return false, nil
}

// MountVolumeSnapshot sets up a read-only mount on top of the snapshot to avoid accidental modifications.
func (d *dir) MountVolumeSnapshot(snapVol Volume, op *operations.Operation) (bool, error) {
	snapPath := snapVol.MountPath()
	return mountReadOnly(snapPath, snapPath)
}

// UnmountVolume simulates unmounting a volume. As dir driver doesn't have volumes to unmount it
// returns false indicating the volume was already unmounted.
func (d *dir) UnmountVolume(vol Volume, op *operations.Operation) (bool, error) {
	return false, nil
}

// UnmountVolumeSnapshot removes the read-only mount placed on top of a snapshot.
func (d *dir) UnmountVolumeSnapshot(snapVol Volume, op *operations.Operation) (bool, error) {
	snapPath := snapVol.MountPath()
	return forceUnmount(snapPath)
}

// SetVolumeQuota sets the quota on the volume.
func (d *dir) SetVolumeQuota(vol Volume, size string, op *operations.Operation) error {
	volPath := vol.MountPath()
	volID, err := d.getVolID(vol.volType, vol.name)
	if err != nil {
		return err
	}

	return d.setQuota(volPath, volID, size)
}

// quotaProjectID generates a project quota ID from a volume ID.
func (d *dir) quotaProjectID(volID int64) uint32 {
	return uint32(volID + 10000)
}

// initQuota initialises the project quota on the path. The volID generates a quota project ID.
func (d *dir) initQuota(path string, volID int64) error {
	if volID == 0 {
		return fmt.Errorf("Missing volume ID")
	}

	ok, err := quota.Supported(path)
	if err != nil || !ok {
		// Skipping quota as underlying filesystem doesn't suppport project quotas.
		return nil
	}

	err = quota.SetProject(path, d.quotaProjectID(volID))
	if err != nil {
		return err
	}

	return nil
}

// setQuota sets the project quota on the path. The volID generates a quota project ID.
func (d *dir) setQuota(path string, volID int64, size string) error {
	if volID == 0 {
		return fmt.Errorf("Missing volume ID")
	}

	// If size not specified in volume config, then use pool's default volume.size setting.
	if size == "" || size == "0" {
		size = d.config["volume.size"]
	}

	sizeBytes, err := units.ParseByteSizeString(size)
	if err != nil {
		return err
	}

	ok, err := quota.Supported(path)
	if err != nil || !ok {
		if sizeBytes > 0 {
			// Skipping quota as underlying filesystem doesn't suppport project quotas.
			d.logger.Warn("The backing filesystem doesn't support quotas, skipping quota", log.Ctx{"path": path})
		}
		return nil
	}

	err = quota.SetProjectQuota(path, d.quotaProjectID(volID), sizeBytes)
	if err != nil {
		return err
	}

	return nil
}

// deleteQuota removes the project quota for a volID from a path.
func (d *dir) deleteQuota(path string, volID int64) error {
	if volID == 0 {
		return fmt.Errorf("Missing volume ID")
	}

	ok, err := quota.Supported(path)
	if err != nil || !ok {
		// Skipping quota as underlying filesystem doesn't suppport project quotas.
		return nil
	}

	err = quota.SetProject(path, 0)
	if err != nil {
		return err
	}

	err = quota.SetProjectQuota(path, d.quotaProjectID(volID), 0)
	if err != nil {
		return err
	}

	return nil
}

// CreateVolumeSnapshot creates a snapshot of a volume.
func (d *dir) CreateVolumeSnapshot(snapVol Volume, op *operations.Operation) error {
	parentName, _, _ := shared.InstanceGetParentAndSnapshotName(snapVol.name)
	srcPath := GetVolumeMountPath(d.name, snapVol.volType, parentName)
	snapPath := snapVol.MountPath()

	// Create the parent directory.
	err := createParentSnapshotDirIfMissing(d.name, snapVol.volType, parentName)
	if err != nil {
		return err
	}

	// Create snapshot directory.
	err = snapVol.CreateMountPath()
	if err != nil {
		return err
	}

	revertPath := true
	defer func() {
		if revertPath {
			os.RemoveAll(snapPath)
		}
	}()

	bwlimit := d.config["rsync.bwlimit"]

	// Copy volume into snapshot directory.
	_, err = rsync.LocalCopy(srcPath, snapPath, bwlimit, true)
	if err != nil {
		return err
	}

	revertPath = false
	return nil
}

// DeleteVolumeSnapshot removes a snapshot from the storage device. The volName and snapshotName
// must be bare names and should not be in the format "volume/snapshot".
func (d *dir) DeleteVolumeSnapshot(snapVol Volume, op *operations.Operation) error {
	snapPath := snapVol.MountPath()

	// Remove the snapshot from the storage device.
	err := os.RemoveAll(snapPath)
	if err != nil {
		return err
	}

	parentName, _, _ := shared.InstanceGetParentAndSnapshotName(snapVol.name)

	// Remove the parent snapshot directory if this is the last snapshot being removed.
	err = deleteParentSnapshotDirIfEmpty(d.name, snapVol.volType, parentName)
	if err != nil {
		return err
	}

	return nil
}

// RenameVolumeSnapshot renames a volume snapshot.
func (d *dir) RenameVolumeSnapshot(snapVol Volume, newSnapshotName string, op *operations.Operation) error {
	return d.vfsRenameVolumeSnapshot(snapVol, newSnapshotName, op)
}

// BackupVolume copies a volume (and optionally its snapshots) to a specified target path.
// This driver does not support optimized backups.
func (d *dir) BackupVolume(vol Volume, targetPath string, _, snapshots bool, op *operations.Operation) error {
	bwlimit := d.config["rsync.bwlimit"]

	var parentVolDir string

	// Backups only implemented for containers currently.
	if vol.volType == VolumeTypeContainer {
		parentVolDir = "container"
	} else {
		return ErrNotImplemented
	}

	// Handle snapshots.
	if snapshots {
		snapshotsPath := filepath.Join(targetPath, "snapshots")
		snapshots, err := vol.Snapshots(op)
		if err != nil {
			return err
		}

		// Create the snapshot path.
		if len(snapshots) > 0 {
			err = os.MkdirAll(snapshotsPath, 0711)
			if err != nil {
				return err
			}
		}

		for _, snap := range snapshots {
			_, snapName, _ := shared.InstanceGetParentAndSnapshotName(snap.Name())
			target := filepath.Join(snapshotsPath, snapName)

			// Copy the snapshot.
			_, err := rsync.LocalCopy(snap.MountPath(), target, bwlimit, true)
			if err != nil {
				return fmt.Errorf("Failed to rsync: %s", err)
			}
		}
	}

	// Copy the parent volume itself.
	target := filepath.Join(targetPath, parentVolDir)
	_, err := rsync.LocalCopy(vol.MountPath(), target, bwlimit, true)
	if err != nil {
		return fmt.Errorf("Failed to rsync: %s", err)
	}

	return nil
}

// RestoreBackupVolume restores a backup tarball onto the storage device.
func (d *dir) RestoreBackupVolume(vol Volume, snapshots []string, srcData io.ReadSeeker, optimizedStorage bool, op *operations.Operation) (func(vol Volume) error, func(), error) {
	revert := true
	revertPaths := []string{}

	// Define a revert function that will be used both to revert if an error occurs inside this
	// function but also return it for use from the calling functions if no error internally.
	revertHook := func() {
		for _, revertPath := range revertPaths {
			os.RemoveAll(revertPath)
		}
	}

	// Only execute the revert function if we have had an error internally and revert is true.
	defer func() {
		if revert {
			revertHook()
		}
	}()

	volPath := vol.MountPath()
	err := vol.CreateMountPath()
	if err != nil {
		return nil, nil, err
	}
	revertPaths = append(revertPaths, volPath)

	// Find the compression algorithm used for backup source data.
	srcData.Seek(0, 0)
	tarArgs, _, _, err := shared.DetectCompressionFile(srcData)
	if err != nil {
		return nil, nil, err
	}

	// Prepare tar extraction arguments.
	args := append(tarArgs, []string{
		"-",
		"--strip-components=2",
		"--xattrs-include=*",
		"-C", volPath, "backup/container",
	}...)

	// Extract instance.
	srcData.Seek(0, 0)
	err = shared.RunCommandWithFds(srcData, nil, "tar", args...)
	if err != nil {
		return nil, nil, err
	}

	if len(snapshots) > 0 {
		// Create new snapshots directory.
		snapshotDir := GetVolumeSnapshotDir(d.name, vol.volType, vol.name)
		err := os.MkdirAll(snapshotDir, 0711)
		if err != nil {
			return nil, nil, err
		}

		revertPaths = append(revertPaths, snapshotDir)

		// Prepare tar arguments.
		args := append(tarArgs, []string{
			"-",
			"--strip-components=2",
			"--xattrs-include=*",
			"-C", snapshotDir, "backup/snapshots",
		}...)

		// Extract snapshots.
		srcData.Seek(0, 0)
		err = shared.RunCommandWithFds(srcData, nil, "tar", args...)
		if err != nil {
			return nil, nil, err
		}
	}

	// Define a post hook function that can be run once the backup config has been restored.
	// This will setup the quota using the restored config.
	postHook := func(vol Volume) error {
		_, err := d.setupInitialQuota(vol)
		return err
	}

	revert = false
	return postHook, revertHook, nil
}
