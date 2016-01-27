package daemon

import (
	"code.google.com/p/go-uuid/uuid"
	"fmt"
	"github.com/Sirupsen/logrus"
	"github.com/rancher/convoy/api"
	"github.com/rancher/convoy/util"
	"net/http"

	. "github.com/rancher/convoy/convoydriver"
	. "github.com/rancher/convoy/logging"
)

func (s *daemon) snapshotExists(volumeUUID, snapshotUUID string) bool {
	volume := s.loadVolume(volumeUUID)
	if volume == nil {
		return false
	}
	_, exists := volume.Snapshots[snapshotUUID]
	return exists
}

func (s *daemon) doSnapshotCreate(version string, w http.ResponseWriter, r *http.Request, objs map[string]string) error {
	s.GlobalLock.Lock()
	defer s.GlobalLock.Unlock()

	request := &api.SnapshotCreateRequest{}
	if err := decodeRequest(r, request); err != nil {
		return err
	}
	volumeUUID := request.VolumeUUID
	if err := util.CheckUUID(volumeUUID); err != nil {
		return err
	}
	snapshotName := request.Name
	if snapshotName != "" {
		if err := util.CheckName(snapshotName); err != nil {
			return err
		}
		existUUID := s.NameUUIDIndex.Get(snapshotName)
		if existUUID != "" {
			return fmt.Errorf("Snapshot name %v already associated with %v", snapshotName, existUUID)
		}
	}

	volume := s.loadVolume(volumeUUID)
	if volume == nil {
		return fmt.Errorf("volume %v doesn't exist", volumeUUID)
	}

	snapOps, err := s.getSnapshotOpsForVolume(volume)
	if err != nil {
		return err
	}

	uuid := uuid.New()
	opts := map[string]string{
		OPT_VOLUME_UUID:   volumeUUID,
		OPT_SNAPSHOT_NAME: snapshotName,
	}

	log.WithFields(logrus.Fields{
		LOG_FIELD_REASON:   LOG_REASON_PREPARE,
		LOG_FIELD_EVENT:    LOG_EVENT_CREATE,
		LOG_FIELD_OBJECT:   LOG_OBJECT_SNAPSHOT,
		LOG_FIELD_SNAPSHOT: uuid,
		LOG_FIELD_VOLUME:   volumeUUID,
	}).Debug()
	if err := snapOps.CreateSnapshot(uuid, opts); err != nil {
		return err
	}
	log.WithFields(logrus.Fields{
		LOG_FIELD_REASON:   LOG_REASON_COMPLETE,
		LOG_FIELD_EVENT:    LOG_EVENT_CREATE,
		LOG_FIELD_OBJECT:   LOG_OBJECT_SNAPSHOT,
		LOG_FIELD_SNAPSHOT: uuid,
		LOG_FIELD_VOLUME:   volumeUUID,
	}).Debug()

	snapshot := Snapshot{
		UUID:       uuid,
		VolumeUUID: volumeUUID,
	}

	//TODO: error handling
	volume.Snapshots[uuid] = snapshot
	if err := s.UUIDIndex.Add(snapshot.UUID); err != nil {
		return err
	}
	if err := s.SnapshotVolumeIndex.Add(snapshot.UUID, volume.UUID); err != nil {
		return err
	}
	if snapshotName != "" {
		if err := s.NameUUIDIndex.Add(snapshotName, snapshot.UUID); err != nil {
			return err
		}
	}
	if err := s.saveVolume(volume); err != nil {
		return err
	}
	driverInfo, err := s.getSnapshotDriverInfo(snapshot.UUID, volume)
	if err != nil {
		return err
	}
	if request.Verbose {
		return writeResponseOutput(w, api.SnapshotResponse{
			UUID:        snapshot.UUID,
			VolumeUUID:  snapshot.VolumeUUID,
			Name:        driverInfo[OPT_SNAPSHOT_NAME],
			CreatedTime: driverInfo[OPT_SNAPSHOT_CREATED_TIME],
			DriverInfo:  driverInfo,
		})
	}
	return writeStringResponse(w, snapshot.UUID)
}

func (s *daemon) getSnapshotDriverInfo(snapshotUUID string, volume *Volume) (map[string]string, error) {
	snapOps, err := s.getSnapshotOpsForVolume(volume)
	if err != nil {
		return nil, err
	}
	driverInfo, err := snapOps.GetSnapshotInfo(snapshotUUID, map[string]string{OPT_VOLUME_UUID: volume.UUID})
	if err != nil {
		return nil, err
	}
	driverInfo["Driver"] = snapOps.Name()
	return driverInfo, nil
}

func (s *daemon) listSnapshotDriverInfos(volume *Volume) (map[string]map[string]string, error) {
	snapOps, err := s.getSnapshotOpsForVolume(volume)
	if err != nil {
		return nil, err
	}
	opts := map[string]string{
		OPT_VOLUME_UUID: volume.UUID,
	}
	snapshots, err := snapOps.ListSnapshot(opts)
	if err != nil {
		return nil, err
	}
	return snapshots, nil
}

func (s *daemon) doSnapshotDelete(version string, w http.ResponseWriter, r *http.Request, objs map[string]string) error {
	s.GlobalLock.Lock()
	defer s.GlobalLock.Unlock()

	request := &api.SnapshotDeleteRequest{}
	if err := decodeRequest(r, request); err != nil {
		return err
	}
	snapshotUUID := request.SnapshotUUID
	if err := util.CheckUUID(snapshotUUID); err != nil {
		return err
	}
	volumeUUID := s.SnapshotVolumeIndex.Get(snapshotUUID)
	if volumeUUID == "" {
		return fmt.Errorf("cannot find volume for snapshot %v", snapshotUUID)
	}

	volume := s.loadVolume(volumeUUID)
	if !s.snapshotExists(volumeUUID, snapshotUUID) {
		return fmt.Errorf("snapshot %v of volume %v doesn't exist", snapshotUUID, volumeUUID)
	}

	snapOps, err := s.getSnapshotOpsForVolume(volume)
	if err != nil {
		return err
	}

	snapshot, err := s.getSnapshotDriverInfo(snapshotUUID, volume)
	if err != nil {
		return err
	}
	snapshotName := snapshot[OPT_SNAPSHOT_NAME]

	log.WithFields(logrus.Fields{
		LOG_FIELD_REASON:   LOG_REASON_PREPARE,
		LOG_FIELD_EVENT:    LOG_EVENT_DELETE,
		LOG_FIELD_OBJECT:   LOG_OBJECT_SNAPSHOT,
		LOG_FIELD_SNAPSHOT: snapshotUUID,
		LOG_FIELD_VOLUME:   volumeUUID,
	}).Debug()
	if err := snapOps.DeleteSnapshot(snapshotUUID, map[string]string{OPT_VOLUME_UUID: volumeUUID}); err != nil {
		return err
	}
	log.WithFields(logrus.Fields{
		LOG_FIELD_REASON:   LOG_REASON_COMPLETE,
		LOG_FIELD_EVENT:    LOG_EVENT_DELETE,
		LOG_FIELD_OBJECT:   LOG_OBJECT_SNAPSHOT,
		LOG_FIELD_SNAPSHOT: snapshotUUID,
		LOG_FIELD_VOLUME:   volumeUUID,
	}).Debug()

	//TODO: error handling
	if err := s.UUIDIndex.Delete(snapshotUUID); err != nil {
		return err
	}
	if err := s.SnapshotVolumeIndex.Delete(snapshotUUID); err != nil {
		return err
	}
	if snapshotName != "" {
		if err := s.NameUUIDIndex.Delete(snapshotName); err != nil {
			return err
		}
	}
	delete(volume.Snapshots, snapshotUUID)
	return s.saveVolume(volume)
}

func (s *daemon) doSnapshotInspect(version string, w http.ResponseWriter, r *http.Request, objs map[string]string) error {
	s.GlobalLock.RLock()
	defer s.GlobalLock.RUnlock()

	request := &api.SnapshotInspectRequest{}
	if err := decodeRequest(r, request); err != nil {
		return err
	}
	snapshotUUID := request.SnapshotUUID
	if err := util.CheckUUID(snapshotUUID); err != nil {
		return err
	}
	volumeUUID := s.SnapshotVolumeIndex.Get(snapshotUUID)
	if volumeUUID == "" {
		return fmt.Errorf("cannot find volume for snapshot %v", snapshotUUID)
	}

	volume := s.loadVolume(volumeUUID)
	if volume == nil {
		return fmt.Errorf("cannot find volume %v", volumeUUID)
	}
	snapshot, err := s.getSnapshotDriverInfo(snapshotUUID, volume)
	if err != nil {
		return fmt.Errorf("cannot find snapshot %v of volume %v", snapshotUUID, volumeUUID)
	}

	driverInfo, err := s.getSnapshotDriverInfo(snapshotUUID, volume)
	if err != nil {
		return err
	}

	resp := api.SnapshotResponse{
		UUID:            snapshotUUID,
		VolumeUUID:      volume.UUID,
		VolumeName:      volume.Name,
		VolumeCreatedAt: volume.CreatedTime,
		Name:            snapshot[OPT_SNAPSHOT_NAME],
		CreatedTime:     snapshot[OPT_SNAPSHOT_CREATED_TIME],
		DriverInfo:      driverInfo,
	}
	data, err := api.ResponseOutput(resp)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}
