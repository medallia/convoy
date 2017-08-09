package nfs

import (
	b64 "encoding/base64"
	"fmt"
	"strings"
	"sync"
	"io/ioutil"
	"os"

	"github.com/Sirupsen/logrus"

	. "github.com/rancher/convoy/convoydriver"
	"github.com/rancher/convoy/util"
	// "github.com/rancher/convoy/util/fs"
	// "strings"
)

var (
	log = logrus.WithFields(logrus.Fields{"pkg": "nfs"})
)

const (
	driverName = "nfs"
	nfsDefaultMountOptions = "nfs.defaultmountoptions"

	defaultMountOptions = ""

	NFS_MOUNTS_DIRECTORY_PERMISSIONS = 0755
)

type Driver struct {
	mutex   *sync.RWMutex
	volumes map[string]*Volume
	*Device
}

type Device struct {
	Root              string
	DefaultMountOptions  []string
}

func (d *Driver) VolumeOps() (VolumeOperations, error) {
	return d, nil
}

func (Driver) SnapshotOps() (SnapshotOperations, error) {
	return nil, fmt.Errorf("Snapshot ops not supported")
}

func (Driver) BackupOps() (BackupOperations, error) {
	return nil, fmt.Errorf("Backup ops not supported")
}

func (d *Driver) Info() (map[string]string, error) {
	return map[string]string{
		"name": d.Name(),
	}, nil
}

func init() {
	if err := Register(driverName, Init); err != nil {
		panic(err)
	}
}

func (*Driver) Name() string {
	return driverName
}

// We never create an NFS volume in internal state
func (d *Driver) CreateVolume(req Request) error {
	return nil
}

func (d *Driver) createVolume(req Request) error {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	v, exists := d.volumes[req.Name]
	if !exists {
		dirName, err := ioutil.TempDir(d.Root, "")
		if err != nil {
			return err
		}
		v = &Volume{
			Name:             req.Name,
			MountPoint: 	  b64.StdEncoding.EncodeToString([]byte(dirName)),
		}
		d.volumes[req.Name] = v
	}
	return nil
}

// We never need to remove a NFS volume from the internal state
func (d *Driver) DeleteVolume(req Request) error {
	// if _, exists := d.volumes[req.Name]; exists {
	// 	delete(d.volumes, req.Name)
	// }
	return nil
}

func (d *Driver) MountVolume(req Request) (string, error) {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	volume, exists := d.volumes[req.Name]
	if exists && volume.MountPoint != "" {
		return volume.MountPoint, nil
	}
	dirName, err := ioutil.TempDir(d.Root, "")
	if err != nil {
		return "", err
	}
	v := &Volume{
		Name:             req.Name,
		MountPoint: 	  dirName,
	}
	d.volumes[req.Name] = v
	mountPoint, err := util.VolumeMount(v, dirName, false)
	log.Debugf("Volume mount error: %+v", err)
	return mountPoint, err
}

func (d *Driver) UmountVolume(req Request) error {
	// volume, exists := d.volumes[req.Name]
	// if !exists {
	// 	return fmt.Errorf("Failed Unmount because %v does not exist in internal state", req.Name)
	// }
	// if err := util.VolumeUmount(volume, "-l"); err != nil {
	// 	return fmt.Errorf("Failed to unmount nfs device=%s from mount=%s - error=%v", volume.Name, volume.MountPoint, err)
	// }
	return nil
}

func (d *Driver) MountPoint(req Request) (string, error) {
	volume, exists := d.volumes[req.Name]
	if !exists {
		return "", fmt.Errorf("Volume=%v is not mounted", req.Name)
	}
	return volume.MountPoint, nil
}

// getCurrentVolumes gets all volumes that are mapped
func (d *Driver) getCurrentVolumes() (map[string]interface{}, error) {
	return map[string]interface{}{}, nil
}

func (d *Driver) GetVolumeInfo(name string) (map[string]string, error) {
	_, exists := d.volumes[name]
	if !exists {
		return nil, util.ErrorNotExists()
	}
	return map[string]string{}, nil
}

func (d *Driver) ListVolume(opts map[string]string) (map[string]map[string]string, error) {
	return map[string]map[string]string{}, nil
}

func Init(root string, config map[string]string) (ConvoyDriver, error) {
	device, err := getDefaultDevice(root, config)
	if err != nil {
		return nil, err
	}
	if err := util.MkdirIfNotExists(root); err != nil {
		return nil, err
	}
	d := &Driver{
		mutex:   &sync.RWMutex{},
		volumes: make(map[string]*Volume),
		Device:  device,
	}
	return d, nil
}

func getDefaultDevice(root string, config map[string]string) (*Device, error) {
	if config[nfsDefaultMountOptions] == "" {
		config[nfsDefaultMountOptions] = defaultMountOptions
	}
	mountOptionsSlice := strings.Split(config[defaultMountOptions], " ")
	dev := &Device{
		DefaultMountOptions: mountOptionsSlice,
		Root:              root,
	}
	return dev, nil
}

type Volume struct {
	m sync.Mutex
	// unique name of the volume
	Name string
	// Host path
	MountPoint string
	// Mount Options
	MountOptions []string
}

func (v *Volume) GetDevice() (string, error) {
	return strings.Replace(v.Name, "//", "://", 1), nil
}

func (v *Volume) GetMountOpts() []string {
	return []string{}
}

func (v *Volume) GenerateDefaultMountPoint() string {
	return v.MountPoint
}

func (v *Volume) Info() map[string]string {
	device, _ := v.GetDevice()

	return map[string]string{
		OPT_VOLUME_NAME: v.Name,
		OPT_MOUNT_POINT: v.MountPoint,
		"Device":        device,
	}
}

func ensureDirectoryExists(dirName string) error {
	_, err := os.Stat(dirName)
	if err == nil {
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	return os.MkdirAll(dirName, NFS_MOUNTS_DIRECTORY_PERMISSIONS)
}
