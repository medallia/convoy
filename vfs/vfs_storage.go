package vfs

import (
	"fmt"
	"github.com/rancher/convoy/convoydriver"
	"github.com/rancher/convoy/objectstore"
	"github.com/rancher/convoy/util"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

const (
	DRIVER_NAME        = "vfs"
	DRIVER_CONFIG_FILE = "vfs.cfg"

	VOLUME_CFG_PREFIX = "volume_"
	VFS_CFG_PREFIX    = DRIVER_NAME + "_"
	CFG_POSTFIX       = ".json"

	SNAPSHOT_PATH = "snapshots"

	VFS_DEFAULT_VOLUME_SIZE = "vfs.defaultvolumesize"
	DEFAULT_VOLUME_SIZE     = "100G"
)

type Driver struct {
	mutex *sync.RWMutex
	Device
}

func init() {
	convoydriver.Register(DRIVER_NAME, Init)
}

func (d *Driver) Name() string {
	return DRIVER_NAME
}

type Device struct {
	Root              string
	Path              string
	DefaultVolumeSize int64
}

func (dev *Device) ConfigFile() (string, error) {
	if dev.Root == "" {
		return "", fmt.Errorf("BUG: Invalid empty device config path")
	}
	return filepath.Join(dev.Root, DRIVER_CONFIG_FILE), nil
}

type Snapshot struct {
	UUID       string
	VolumeUUID string
	FilePath   string
}

type Volume struct {
	UUID         string
	Size         int64
	Path         string
	MountPoint   string
	PrepareForVM bool
	Snapshots    map[string]Snapshot

	configPath string
}

func (v *Volume) ConfigFile() (string, error) {
	if v.UUID == "" {
		return "", fmt.Errorf("BUG: Invalid empty volume UUID")
	}
	if v.configPath == "" {
		return "", fmt.Errorf("BUG: Invalid empty volume config path")
	}
	return filepath.Join(v.configPath, VFS_CFG_PREFIX+VOLUME_CFG_PREFIX+v.UUID+CFG_POSTFIX), nil
}

func (device *Device) listVolumeIDs() ([]string, error) {
	return util.ListConfigIDs(device.Root, VFS_CFG_PREFIX+VOLUME_CFG_PREFIX, CFG_POSTFIX)
}

func Init(root string, config map[string]string) (convoydriver.ConvoyDriver, error) {
	dev := &Device{
		Root: root,
	}
	exists, err := util.ObjectExists(dev)
	if err != nil {
		return nil, err
	}
	if exists {
		if err := util.ObjectLoad(dev); err != nil {
			return nil, err
		}
	} else {
		if err := util.MkdirIfNotExists(root); err != nil {
			return nil, err
		}

		path := config[VFS_PATH]
		if path == "" {
			return nil, fmt.Errorf("VFS driver base path unspecified")
		}
		if err := util.MkdirIfNotExists(path); err != nil {
			return nil, err
		}

		dev = &Device{
			Root: root,
			Path: path,
		}

		if _, exists := config[VFS_DEFAULT_VOLUME_SIZE]; !exists {
			config[VFS_DEFAULT_VOLUME_SIZE] = DEFAULT_VOLUME_SIZE
		}
		volumeSize, err := util.ParseSize(config[VFS_DEFAULT_VOLUME_SIZE])
		if err != nil || volumeSize == 0 {
			return nil, fmt.Errorf("Illegal default volume size specified")
		}
		dev.DefaultVolumeSize = volumeSize
	}

	// For upgrade case
	if dev.DefaultVolumeSize == 0 {
		dev.DefaultVolumeSize, err = util.ParseSize(DEFAULT_VOLUME_SIZE)
		if err != nil || dev.DefaultVolumeSize == 0 {
			return nil, fmt.Errorf("Illegal default volume size specified")
		}
	}

	if err := util.ObjectSave(dev); err != nil {
		return nil, err
	}
	d := &Driver{
		mutex:  &sync.RWMutex{},
		Device: *dev,
	}

	return d, nil
}

func (d *Driver) Info() (map[string]string, error) {
	return map[string]string{
		"Root":              d.Root,
		"Path":              d.Path,
		"DefaultVolumeSize": strconv.FormatInt(d.DefaultVolumeSize, 10),
	}, nil
}

func (d *Driver) VolumeOps() (convoydriver.VolumeOperations, error) {
	return d, nil
}

func (d *Driver) blankVolume(id string) *Volume {
	return &Volume{
		configPath: d.Root,
		UUID:       id,
	}
}

func (d *Driver) getSize(opts map[string]string, defaultVolumeSize int64) (int64, error) {
	size := opts[convoydriver.OPT_SIZE]
	if size == "" || size == "0" {
		size = strconv.FormatInt(defaultVolumeSize, 10)
	}
	return util.ParseSize(size)
}

func (d *Driver) CreateVolume(id string, opts map[string]string) error {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	backupURL := opts[convoydriver.OPT_BACKUP_URL]
	if backupURL != "" {
		objVolume, err := objectstore.LoadVolume(backupURL)
		if err != nil {
			return err
		}
		if objVolume.Driver != d.Name() {
			return fmt.Errorf("Cannot restore backup of %v to %v", objVolume.Driver, d.Name())
		}
	}

	volumeName := opts[convoydriver.OPT_VOLUME_NAME]
	if volumeName == "" {
		volumeName = "volume-" + id[:8]
	}

	volume := d.blankVolume(id)
	exists, err := util.ObjectExists(volume)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("volume %v already exists", id)
	}

	volume.PrepareForVM, err = strconv.ParseBool(opts[convoydriver.OPT_PREPARE_FOR_VM])
	if err != nil {
		return err
	}
	if volume.PrepareForVM {
		volume.Size, err = d.getSize(opts, d.DefaultVolumeSize)
		if err != nil {
			return err
		}
	}

	volumePath := filepath.Join(d.Path, volumeName)
	if err := util.MkdirIfNotExists(volumePath); err != nil {
		return err
	}
	volume.Path = volumePath
	volume.Snapshots = make(map[string]Snapshot)

	if backupURL != "" {
		file, err := objectstore.RestoreSingleFileBackup(backupURL, volumePath)
		if err != nil {
			return err
		}
		// file would be removed after this because it's under volumePath
		if err := util.DecompressDir(file, volumePath); err != nil {
			return err
		}
	}
	return util.ObjectSave(volume)
}

func (d *Driver) DeleteVolume(id string, opts map[string]string) error {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	volume := d.blankVolume(id)
	if err := util.ObjectLoad(volume); err != nil {
		return err
	}

	if volume.MountPoint != "" {
		return fmt.Errorf("Cannot delete volume %v. It is still mounted", id)
	}
	referenceOnly, _ := strconv.ParseBool(opts[convoydriver.OPT_REFERENCE_ONLY])
	if !referenceOnly {
		log.Debugf("Cleaning up %v for volume %v", volume.Path, id)
		if out, err := util.Execute("rm", []string{"-rf", volume.Path}); err != nil {
			return fmt.Errorf("Fail to cleanup the volume, output: %v, error: %v", out, err.Error())
		}
	}
	return util.ObjectDelete(volume)
}

func (d *Driver) MountVolume(id string, opts map[string]string) (string, error) {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	volume := d.blankVolume(id)
	if err := util.ObjectLoad(volume); err != nil {
		return "", err
	}

	specifiedPoint := opts[convoydriver.OPT_MOUNT_POINT]
	if specifiedPoint != "" {
		return "", fmt.Errorf("VFS doesn't support specified mount point")
	}
	if volume.MountPoint == "" {
		volume.MountPoint = volume.Path
	}
	if volume.PrepareForVM {
		if err := util.MountPointPrepareImageFile(volume.MountPoint, volume.Size); err != nil {
			return "", err
		}
	}
	if err := util.ObjectSave(volume); err != nil {
		return "", err
	}
	return volume.MountPoint, nil
}

func (d *Driver) UmountVolume(id string) error {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	volume := d.blankVolume(id)
	if err := util.ObjectLoad(volume); err != nil {
		return err
	}

	if volume.MountPoint != "" {
		volume.MountPoint = ""
	}
	return util.ObjectSave(volume)
}

func (d *Driver) ListVolume(opts map[string]string) (map[string]map[string]string, error) {
	d.mutex.RLock()
	defer d.mutex.RUnlock()

	volumeIDs, err := d.listVolumeIDs()
	if err != nil {
		return nil, err
	}
	result := map[string]map[string]string{}
	for _, id := range volumeIDs {
		result[id], err = d.GetVolumeInfo(id)
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (d *Driver) GetVolumeInfo(id string) (map[string]string, error) {
	d.mutex.RLock()
	defer d.mutex.RUnlock()

	volume := d.blankVolume(id)
	if err := util.ObjectLoad(volume); err != nil {
		return nil, err
	}

	size := "0"
	prepareForVM := strconv.FormatBool(volume.PrepareForVM)
	if volume.PrepareForVM {
		size = strconv.FormatInt(volume.Size, 10)
	}
	return map[string]string{
		"Path": volume.Path,
		convoydriver.OPT_MOUNT_POINT:    volume.MountPoint,
		convoydriver.OPT_SIZE:           size,
		convoydriver.OPT_PREPARE_FOR_VM: prepareForVM,
	}, nil
}

func (d *Driver) MountPoint(id string) (string, error) {
	d.mutex.RLock()
	defer d.mutex.RUnlock()

	volume := d.blankVolume(id)
	if err := util.ObjectLoad(volume); err != nil {
		return "", err
	}
	return volume.MountPoint, nil
}

func (d *Driver) SnapshotOps() (convoydriver.SnapshotOperations, error) {
	return d, nil
}

func (d *Driver) getSnapshotFilePath(snapshotID, volumeID string) string {
	return filepath.Join(d.Root, SNAPSHOT_PATH, volumeID+"_"+snapshotID+".tar.gz")
}

func (d *Driver) CreateSnapshot(id, volumeID string) error {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	volume := d.blankVolume(volumeID)
	if err := util.ObjectLoad(volume); err != nil {
		return err
	}
	if _, exists := volume.Snapshots[id]; exists {
		return fmt.Errorf("Snapshot %v already exists for volume %v", id, volumeID)
	}
	snapFile := d.getSnapshotFilePath(id, volumeID)
	if err := util.MkdirIfNotExists(filepath.Dir(snapFile)); err != nil {
		return err
	}
	if err := util.CompressDir(volume.Path, snapFile); err != nil {
		return err
	}
	volume.Snapshots[id] = Snapshot{
		UUID:       id,
		VolumeUUID: volumeID,
		FilePath:   snapFile,
	}
	return util.ObjectSave(volume)
}

func (d *Driver) DeleteSnapshot(id, volumeID string) error {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	volume := d.blankVolume(volumeID)
	if err := util.ObjectLoad(volume); err != nil {
		return err
	}
	snapshot, exists := volume.Snapshots[id]
	if !exists {
		return fmt.Errorf("Snapshot %v doesn't exists for volume %v", id, volumeID)
	}
	if err := os.Remove(snapshot.FilePath); err != nil {
		return err
	}
	delete(volume.Snapshots, id)
	return util.ObjectSave(volume)
}

func (d *Driver) GetSnapshotInfo(id, volumeID string) (map[string]string, error) {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	volume := d.blankVolume(volumeID)
	if err := util.ObjectLoad(volume); err != nil {
		return nil, err
	}
	snapshot, exists := volume.Snapshots[id]
	if !exists {
		return nil, fmt.Errorf("Snapshot %v doesn't exists for volume %v", id, volumeID)
	}
	return map[string]string{
		"UUID":       snapshot.UUID,
		"VolumeUUID": snapshot.VolumeUUID,
		"FilePath":   snapshot.FilePath,
	}, nil
}

func (d *Driver) ListSnapshot(opts map[string]string) (map[string]map[string]string, error) {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	var (
		volumeIDs []string
		err       error
	)
	snapshots := make(map[string]map[string]string)
	specifiedVolumeID := opts["VolumeID"]
	if specifiedVolumeID != "" {
		volumeIDs = []string{
			specifiedVolumeID,
		}
	} else {
		volumeIDs, err = d.listVolumeIDs()
		if err != nil {
			return nil, err
		}
	}
	for _, volumeID := range volumeIDs {
		volume := d.blankVolume(volumeID)
		if err := util.ObjectLoad(volume); err != nil {
			return nil, err
		}
		for snapshotID := range volume.Snapshots {
			snapshots[snapshotID], err = d.GetSnapshotInfo(snapshotID, volumeID)
			if err != nil {
				return nil, err
			}
		}
	}
	return snapshots, nil
}

func (d *Driver) BackupOps() (convoydriver.BackupOperations, error) {
	return d, nil
}

func (d *Driver) CreateBackup(snapshotID, volumeID, destURL string, opts map[string]string) (string, error) {
	volume := d.blankVolume(volumeID)
	if err := util.ObjectLoad(volume); err != nil {
		return "", err
	}
	snapshot, exists := volume.Snapshots[snapshotID]
	if !exists {
		return "", fmt.Errorf("Cannot find snapshot %v for volume %v", snapshotID, volumeID)
	}
	objVolume := &objectstore.Volume{
		UUID:        volumeID,
		Name:        opts[convoydriver.OPT_VOLUME_NAME],
		Driver:      d.Name(),
		FileSystem:  opts[convoydriver.OPT_FILESYSTEM],
		CreatedTime: opts[convoydriver.OPT_VOLUME_CREATED_TIME],
	}
	objSnapshot := &objectstore.Snapshot{
		UUID:        snapshotID,
		Name:        opts[convoydriver.OPT_SNAPSHOT_NAME],
		CreatedTime: opts[convoydriver.OPT_SNAPSHOT_CREATED_TIME],
	}
	return objectstore.CreateSingleFileBackup(objVolume, objSnapshot, snapshot.FilePath, destURL)
}

func (d *Driver) DeleteBackup(backupURL string) error {
	objVolume, err := objectstore.LoadVolume(backupURL)
	if err != nil {
		return err
	}
	if objVolume.Driver != d.Name() {
		return fmt.Errorf("BUG: Wrong driver handling DeleteBackup(), driver should be %v but is %v", objVolume.Driver, d.Name())
	}
	return objectstore.DeleteSingleFileBackup(backupURL)
}

func (d *Driver) GetBackupInfo(backupURL string) (map[string]string, error) {
	objVolume, err := objectstore.LoadVolume(backupURL)
	if err != nil {
		return nil, err
	}
	if objVolume.Driver != d.Name() {
		return nil, fmt.Errorf("BUG: Wrong driver handling DeleteBackup(), driver should be %v but is %v", objVolume.Driver, d.Name())
	}
	return objectstore.GetBackupInfo(backupURL)
}

func (d *Driver) ListBackup(destURL string, opts map[string]string) (map[string]map[string]string, error) {
	return objectstore.List(opts[convoydriver.OPT_VOLUME_UUID], destURL, d.Name())
}
