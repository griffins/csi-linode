package driver

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"encoding/json"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi/v0"
	"github.com/linode/linodego"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Size Constants
const (
	_  = iota
	KB = 1 << (10 * iota)
	MB
	GB
	TB
)

const (
	defaultVolumeSizeInGB = 10 * GB
)

const waitTimeout = 60

// CreateVolume creates a new volume from the given request. The function is
// idempotent.
func (d *Driver) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "CreateVolume Name must be provided")
	}

	if req.VolumeCapabilities == nil || len(req.VolumeCapabilities) == 0 {
		return nil, status.Error(codes.InvalidArgument, "CreateVolume Volume capabilities must be provided")
	}

	size, err := extractStorage(req.CapacityRange)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	volumeName := strings.Replace(req.Name, "-", "", -1)
	if len(volumeName) > 32 {
		volumeName = volumeName[:32]
	}

	ll := d.log.WithFields(logrus.Fields{
		"volume_name":             volumeName,
		"storage_size_giga_bytes": size / GB,
		"method":                  "create_volume",
	})
	ll.Info("create volume called")

	// get volume first, if it's created do no thing

	jsonFilter, err := json.Marshal(map[string]string{"label": volumeName})
	if err != nil {
		return nil, err
	}

	volumes, err := d.linodeClient.ListVolumes(ctx, linodego.NewListOptions(0, string(jsonFilter)))
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	// volume already exist, do nothing
	if len(volumes) != 0 {
		if len(volumes) > 1 {
			return nil, status.Error(codes.AlreadyExists, fmt.Sprintf("duplicate volume %q exists", volumeName))
		}
		volume := volumes[0]
		if int64(volume.Size*GB) != size {
			return nil, status.Error(codes.AlreadyExists, fmt.Sprintf("invalid option requested size: %d", size))
		}

		ll.Info("volume already created")
		return &csi.CreateVolumeResponse{
			Volume: &csi.Volume{
				Id:            strconv.Itoa(volume.ID),
				CapacityBytes: int64(volume.Size * GB),
			},
		}, nil
	}

	volumeReq := linodego.VolumeCreateOptions{
		Region: d.region,
		Label:  volumeName,
		Size:   int(size / GB),
	}

	ll.WithField("volume_req", volumeReq).Info("creating volume")

	vol, err := d.linodeClient.CreateVolume(ctx, volumeReq)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	vol, err = d.linodeClient.WaitForVolumeStatus(ctx, vol.ID, linodego.VolumeActive, waitTimeout)

	if err != nil {
		return nil, err
	}

	ll.WithField("vol", vol).Info("volume active")

	resp := &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			Id:            strconv.Itoa(vol.ID),
			CapacityBytes: size,
		},
	}

	ll.WithField("response", resp).Info("volume created")
	return resp, nil
}

// DeleteVolume deletes the given volume. The function is idempotent.
func (d *Driver) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "DeleteVolume Volume ID must be provided")
	}

	ll := d.log.WithFields(logrus.Fields{
		"volume_id": req.VolumeId,
		"method":    "delete_volume",
	})
	ll.Info("delete volume called")
	volID, err := strconv.Atoi(req.VolumeId)
	if err != nil {
		return nil, err
	}

	vol, err := d.linodeClient.GetVolume(ctx, volID)
	if vol == nil {
		// we assume it's deleted already for idempotency
		return &csi.DeleteVolumeResponse{}, nil
	}
	if err != nil {
		return nil, err
	}

	err = d.linodeClient.DeleteVolume(ctx, volID)
	if err != nil {
		return nil, err
	}

	ll.Info("volume is deleted")
	return &csi.DeleteVolumeResponse{}, nil
}

// ControllerPublishVolume attaches the given volume to the node
func (d *Driver) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "ControllerPublishVolume Volume ID must be provided")
	}

	if req.NodeId == "" {
		return nil, status.Error(codes.InvalidArgument, "ControllerPublishVolume Node ID must be provided")
	}

	if req.VolumeCapability == nil {
		return nil, status.Error(codes.InvalidArgument, "ControllerPublishVolume Volume capability must be provided")
	}

	linodeID, err := strconv.Atoi(req.NodeId)
	if err != nil {
		return nil, status.Error(codes.Unknown, fmt.Sprintf("malformed nodeId %q detected: %s", req.NodeId, err))
	}

	ll := d.log.WithFields(logrus.Fields{
		"volume_id": req.VolumeId,
		"node_id":   req.NodeId,
		"linode_id": linodeID,
		"method":    "controller_publish_volume",
	})
	ll.Info("controller publish volume called")

	volumeID, err := strconv.Atoi(req.VolumeId)
	if err != nil {
		return nil, err
	}

	volume, err := d.linodeClient.GetVolume(ctx, volumeID)
	if volume == nil {
		return nil, status.Error(codes.NotFound, fmt.Sprintf("Volume with id %v not found", volumeID))
	}
	if err != nil {
		return nil, err
	}
	if volume.LinodeID != nil {
		return nil, status.Error(codes.AlreadyExists, fmt.Sprintf("Volume with id %v already attached", volumeID))
	}

	linode, err := d.linodeClient.GetInstance(ctx, linodeID)
	if linode == nil {
		return nil, status.Error(codes.NotFound, fmt.Sprintf("Linode instance with id %v not found", linodeID))
	}
	if err != nil {
		return nil, err
	}

	opts := &linodego.VolumeAttachOptions{
		LinodeID: linodeID,
		ConfigID: 0,
	}

	_, err = d.linodeClient.AttachVolume(ctx, volumeID, opts)
	if err != nil {
		return nil, fmt.Errorf("error attaching volume: %s", err)
	}
	if err != nil {
		return nil, err
	}

	ll.Infoln("waiting for attaching volume")
	if volume, err = d.linodeClient.WaitForVolumeLinodeID(ctx, volumeID, &linodeID, waitTimeout); err != nil {
		return nil, err
	}

	ll.Info("volume %d is attached to instance %d", volume.ID, *volume.LinodeID)
	return &csi.ControllerPublishVolumeResponse{}, nil
}

// ControllerUnpublishVolume deattaches the given volume from the node
func (d *Driver) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "ControllerPublishVolume Volume ID must be provided")
	}

	linodeID, err := strconv.Atoi(req.NodeId)
	if err != nil {
		return nil, fmt.Errorf("malformed nodeId %q detected: %s", req.NodeId, err)
	}

	ll := d.log.WithFields(logrus.Fields{
		"volume_id": req.VolumeId,
		"node_id":   req.NodeId,
		"linode_id": linodeID,
		"method":    "controller_unpublish_volume",
	})
	ll.Info("controller unpublish volume called")

	volumeID, err := strconv.Atoi(req.VolumeId)
	if err != nil {
		return nil, err
	}

	err = d.linodeClient.DetachVolume(ctx, volumeID)
	if err != nil {
		return nil, fmt.Errorf("Error detaching volume: %s", err)
	}

	ll.Infoln("waiting for detaching volume")
	if _, err = d.linodeClient.WaitForVolumeLinodeID(ctx, volumeID, nil, waitTimeout); err != nil {
		return nil, err
	}

	ll.Info("volume is detached")
	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

// ValidateVolumeCapabilities checks whether the volume capabilities requested
// are supported.
func (d *Driver) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "ValidateVolumeCapabilities Volume ID must be provided")
	}

	if req.VolumeCapabilities == nil {
		return nil, status.Error(codes.InvalidArgument, "ValidateVolumeCapabilities Volume Capabilities must be provided")
	}

	volumeID, err := strconv.Atoi(req.VolumeId)
	if err != nil {
		return nil, err
	}

	volume, err := d.linodeClient.GetVolume(ctx, volumeID)
	if volume == nil {
		return nil, status.Error(codes.NotFound, fmt.Sprintf("Volume with id %v not found", volumeID))
	}
	if err != nil {
		return nil, err
	}

	var vcaps []*csi.VolumeCapability_AccessMode
	for _, mode := range []csi.VolumeCapability_AccessMode_Mode{
		// Linode only supports any volume to be attached to a single
		// node in read/write mode
		csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
	} {
		vcaps = append(vcaps, &csi.VolumeCapability_AccessMode{Mode: mode})
	}

	ll := d.log.WithFields(logrus.Fields{
		"volume_id":              req.VolumeId,
		"volume_capabilities":    req.VolumeCapabilities,
		"supported_capabilities": vcaps,
		"method":                 "validate_volume_capabilities",
	})
	ll.Info("validate volume capabilities called")

	hasSupport := func(mode csi.VolumeCapability_AccessMode_Mode) bool {
		for _, m := range vcaps {
			if mode == m.Mode {
				return true
			}
		}
		return false
	}

	resp := &csi.ValidateVolumeCapabilitiesResponse{
		Supported: false,
	}

	for _, capabilities := range req.VolumeCapabilities {
		if hasSupport(capabilities.AccessMode.Mode) {
			resp.Supported = true
		} else {
			// we need to make sure all capabilities are supported. Revert back
			// in case we have a cap that is supported, but is invalidated now
			resp.Supported = false
		}
	}

	ll.WithField("response", resp).Info("supported capabilities")
	return resp, nil
}

// ListVolumes returns a list of all requested volumes
func (d *Driver) ListVolumes(ctx context.Context, req *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
	//var page int
	var err error
	/*if req.StartingToken != "" {
		page, err = strconv.Atoi(req.StartingToken)
		if err != nil {
			return nil, err
		}
	}*/

	listOpts := linodego.NewListOptions(0, "")
	ll := d.log.WithFields(logrus.Fields{
		"list_opts":          listOpts,
		"req_starting_token": req.StartingToken,
		"method":             "list_volumes",
	})
	ll.Info("list volumes called")

	// TODO(sanjid) : understand paginate here
	var volumes []linodego.Volume
	//lastPage := 0

	volumes, err = d.linodeClient.ListVolumes(ctx, listOpts)
	if err != nil {
		return nil, err
	}

	var entries []*csi.ListVolumesResponse_Entry
	for _, vol := range volumes {
		entries = append(entries, &csi.ListVolumesResponse_Entry{
			Volume: &csi.Volume{
				Id:            strconv.Itoa(vol.ID),
				CapacityBytes: int64(vol.Size * GB),
			},
		})
	}

	// TODO(displague): check that the NextToken logic works fine, might be racy
	resp := &csi.ListVolumesResponse{
		Entries: entries,
		//NextToken: strconv.Itoa(lastPage),
	}

	ll.WithField("response", resp).Info("volumes listed")
	return resp, nil
}

// GetCapacity returns the capacity of the storage pool
func (d *Driver) GetCapacity(ctx context.Context, req *csi.GetCapacityRequest) (*csi.GetCapacityResponse, error) {
	// TODO(displague): check if we can provide this information somehow
	d.log.WithFields(logrus.Fields{
		"params": req.Parameters,
		"method": "get_capacity",
	}).Warn("get capacity is not implemented")
	return nil, status.Error(codes.Unimplemented, "")
}

// ControllerGetCapabilities returns the capabilities of the controller service.
func (d *Driver) ControllerGetCapabilities(ctx context.Context, req *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	newCap := func(cap csi.ControllerServiceCapability_RPC_Type) *csi.ControllerServiceCapability {
		return &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: cap,
				},
			},
		}
	}

	// TODO(displague): checkout if the capabilities are worth supporting
	var caps []*csi.ControllerServiceCapability
	for _, capability := range []csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
		csi.ControllerServiceCapability_RPC_LIST_VOLUMES,
	} {
		caps = append(caps, newCap(capability))
	}

	resp := &csi.ControllerGetCapabilitiesResponse{
		Capabilities: caps,
	}

	d.log.WithFields(logrus.Fields{
		"response": resp,
		"method":   "controller_get_capabilities",
	}).Info("controller get capabilities called")
	return resp, nil
}

// CreateSnapshot Create Snapshot
func (d *Driver) CreateSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	return nil, errors.New("not implemented")
}

// DeleteSnapshot Delete Snapshot
func (d *Driver) DeleteSnapshot(ctx context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	return nil, errors.New("not implemented")
}

// ListSnapshots List Snapshots
func (d *Driver) ListSnapshots(ctx context.Context, req *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	return nil, errors.New("not implemented")
}

// extractStorage extracts the storage size in GB from the given capacity
// range. If the capacity range is not satisfied it returns the default volume
// size.
func extractStorage(capRange *csi.CapacityRange) (int64, error) {
	if capRange == nil {
		return defaultVolumeSizeInGB, nil
	}

	if capRange.RequiredBytes == 0 && capRange.LimitBytes == 0 {
		return defaultVolumeSizeInGB, nil
	}

	minSize := capRange.RequiredBytes

	// limitBytes might be zero
	maxSize := capRange.LimitBytes
	if capRange.LimitBytes == 0 {
		maxSize = minSize
	}

	if minSize == maxSize {
		return minSize, nil
	}

	return 0, errors.New("requiredBytes and LimitBytes are not the same")
}
