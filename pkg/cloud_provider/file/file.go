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

package file

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/golang/glog"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/gcp-filestore-csi-driver/pkg/util"

	filev1 "google.golang.org/api/file/v1"
	filev1beta1 "google.golang.org/api/file/v1beta1"
)

type ServiceInstance struct {
	Project  string
	Name     string
	Location string
	Tier     string
	Network  Network
	Volume   Volume
	Labels   map[string]string
	State    string
}

type Volume struct {
	Name      string
	SizeBytes int64
}

type Network struct {
	Name            string
	ReservedIpRange string
	Ip              string
}

type BackupInfo struct {
	Backup             *filev1beta1.Backup
	SourceVolumeHandle string
}

type Service interface {
	CreateInstance(ctx context.Context, obj *ServiceInstance) (*ServiceInstance, error)
	DeleteInstance(ctx context.Context, obj *ServiceInstance) error
	GetInstance(ctx context.Context, obj *ServiceInstance) (*ServiceInstance, error)
	ListInstances(ctx context.Context, obj *ServiceInstance) ([]*ServiceInstance, error)
	ResizeInstance(ctx context.Context, obj *ServiceInstance) (*ServiceInstance, error)
	GetBackup(ctx context.Context, backupUri string) (*BackupInfo, error)
	CreateBackup(ctx context.Context, obj *ServiceInstance, backupId, backupLocation string) (*filev1beta1.Backup, error)
	DeleteBackup(ctx context.Context, backupId string) error
	CreateInstanceFromBackupSource(ctx context.Context, obj *ServiceInstance, volumeSourceSnapshotId string) (*ServiceInstance, error)
}

type gcfsServiceManager struct {
	fileService              *filev1.Service
	instancesService         *filev1.ProjectsLocationsInstancesService
	operationsService        *filev1.ProjectsLocationsOperationsService
	instancesV1beta1Service  *filev1beta1.ProjectsLocationsInstancesService
	operationsV1beta1Service *filev1beta1.ProjectsLocationsOperationsService
	backupService            *filev1beta1.ProjectsLocationsBackupsService
}

const (
	locationURIFmt  = "projects/%s/locations/%s"
	instanceURIFmt  = locationURIFmt + "/instances/%s"
	operationURIFmt = locationURIFmt + "/operations/%s"
	backupURIFmt    = locationURIFmt + "/backups/%s"

	// Patch update masks
	fileShareUpdateMask = "file_shares"
)

var _ Service = &gcfsServiceManager{}

func NewGCFSService(version string, client *http.Client) (Service, error) {
	ctx := context.Background()
	fileService, err := filev1.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		return nil, err
	}
	fileService.UserAgent = fmt.Sprintf("Google Cloud Filestore CSI Driver/%s (%s %s)", version, runtime.GOOS, runtime.GOARCH)

	fileV1beta1Service, err := filev1beta1.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		return nil, err
	}
	fileV1beta1Service.UserAgent = fmt.Sprintf("Google Cloud Filestore CSI Driver/%s (%s %s)", version, runtime.GOOS, runtime.GOARCH)
	return &gcfsServiceManager{
		fileService:              fileService,
		instancesService:         filev1.NewProjectsLocationsInstancesService(fileService),
		operationsService:        filev1.NewProjectsLocationsOperationsService(fileService),
		instancesV1beta1Service:  filev1beta1.NewProjectsLocationsInstancesService(fileV1beta1Service),
		operationsV1beta1Service: filev1beta1.NewProjectsLocationsOperationsService(fileV1beta1Service),
		backupService:            filev1beta1.NewProjectsLocationsBackupsService(fileV1beta1Service),
	}, nil
}

func (manager *gcfsServiceManager) CreateInstance(ctx context.Context, obj *ServiceInstance) (*ServiceInstance, error) {
	betaObj := &filev1.Instance{
		Tier: obj.Tier,
		FileShares: []*filev1.FileShareConfig{
			{
				Name:       obj.Volume.Name,
				CapacityGb: util.RoundBytesToGb(obj.Volume.SizeBytes),
			},
		},
		Networks: []*filev1.NetworkConfig{
			{
				Network:         obj.Network.Name,
				Modes:           []string{"MODE_IPV4"},
				ReservedIpRange: obj.Network.ReservedIpRange,
			},
		},
		Labels: obj.Labels,
	}

	glog.V(4).Infof("Creating instance %v: location %v, tier %v, capacity %v, network %v, ipRange %v, labels %v",
		obj.Name,
		obj.Location,
		betaObj.Tier,
		betaObj.FileShares[0].CapacityGb,
		betaObj.Networks[0].Network,
		betaObj.Networks[0].ReservedIpRange,
		betaObj.Labels)
	op, err := manager.instancesService.Create(locationURI(obj.Project, obj.Location), betaObj).InstanceId(obj.Name).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("CreateInstance operation failed: %v", err)
	}

	glog.V(4).Infof("For instance %v, waiting for create instance op %v to complete", obj.Name, op.Name)
	err = manager.waitForOp(ctx, op)
	if err != nil {
		return nil, fmt.Errorf("WaitFor CreateInstance operation failed: %v", err)
	}
	instance, err := manager.GetInstance(ctx, obj)
	if err != nil {
		return nil, fmt.Errorf("failed to get instance after creation: %v", err)
	}
	return instance, nil
}

func (manager *gcfsServiceManager) CreateInstanceFromBackupSource(ctx context.Context, obj *ServiceInstance, sourceSnapshotId string) (*ServiceInstance, error) {
	instance := &filev1beta1.Instance{
		Tier: obj.Tier,
		FileShares: []*filev1beta1.FileShareConfig{
			{
				Name:         obj.Volume.Name,
				CapacityGb:   util.RoundBytesToGb(obj.Volume.SizeBytes),
				SourceBackup: sourceSnapshotId,
			},
		},
		Networks: []*filev1beta1.NetworkConfig{
			{
				Network:         obj.Network.Name,
				Modes:           []string{"MODE_IPV4"},
				ReservedIpRange: obj.Network.ReservedIpRange,
			},
		},
		Labels: obj.Labels,
		State:  obj.State,
	}

	glog.V(4).Infof("Creating instance %v: location %v, tier %v, capacity %v, network %v, ipRange %v, labels %v backup source %v",
		obj.Name,
		obj.Location,
		instance.Tier,
		instance.FileShares[0].CapacityGb,
		instance.Networks[0].Network,
		instance.Networks[0].ReservedIpRange,
		instance.Labels,
		instance.FileShares[0].SourceBackup)
	op, err := manager.instancesV1beta1Service.Create(locationURI(obj.Project, obj.Location), instance).InstanceId(obj.Name).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("CreateInstance operation failed: %v", err)
	}

	glog.V(4).Infof("For instance %v, waiting for create instance op %v to complete", obj.Name, op.Name)
	err = manager.waitForV1beta1Op(ctx, op)
	if err != nil {
		return nil, fmt.Errorf("WaitFor CreateInstance operation failed: %v", err)
	}
	serviceInstance, err := manager.GetInstance(ctx, obj)
	if err != nil {
		return nil, fmt.Errorf("failed to get instance after creation: %v", err)
	}
	return serviceInstance, nil
}

func (manager *gcfsServiceManager) GetInstance(ctx context.Context, obj *ServiceInstance) (*ServiceInstance, error) {
	instanceUri := instanceURI(obj.Project, obj.Location, obj.Name)
	instance, err := manager.instancesService.Get(instanceUri).Context(ctx).Do()
	if err != nil {
		glog.Errorf("Failed to get instance %v", instanceUri)
		return nil, err
	}

	if instance != nil {
		glog.V(5).Infof("GetInstance call fetched instance %+v", instance)
		return cloudInstanceToServiceInstance(instance)
	}
	return nil, fmt.Errorf("failed to get instance %v", instanceUri)
}

func cloudInstanceToServiceInstance(instance *filev1.Instance) (*ServiceInstance, error) {
	project, location, name, err := getInstanceNameFromURI(instance.Name)
	if err != nil {
		return nil, err
	}
	return &ServiceInstance{
		Project:  project,
		Location: location,
		Name:     name,
		Tier:     instance.Tier,
		Volume: Volume{
			Name:      instance.FileShares[0].Name,
			SizeBytes: util.GbToBytes(instance.FileShares[0].CapacityGb),
		},
		Network: Network{
			Name:            instance.Networks[0].Network,
			Ip:              instance.Networks[0].IpAddresses[0],
			ReservedIpRange: instance.Networks[0].ReservedIpRange,
		},
		Labels: instance.Labels,
		State:  instance.State,
	}, nil
}

func CompareInstances(a, b *ServiceInstance) error {
	mismatches := []string{}
	if strings.ToLower(a.Tier) != strings.ToLower(b.Tier) {
		mismatches = append(mismatches, "tier")
	}
	if a.Volume.Name != b.Volume.Name {
		mismatches = append(mismatches, "volume name")
	}
	if util.RoundBytesToGb(a.Volume.SizeBytes) != util.RoundBytesToGb(b.Volume.SizeBytes) {
		mismatches = append(mismatches, "volume size")
	}
	if a.Network.Name != b.Network.Name {
		mismatches = append(mismatches, "network name")
	}

	if len(mismatches) > 0 {
		return fmt.Errorf("instance %v already exists but doesn't match expected: %+v", a.Name, mismatches)
	}
	return nil
}

func (manager *gcfsServiceManager) DeleteInstance(ctx context.Context, obj *ServiceInstance) error {
	instance, err := manager.GetInstance(ctx, obj)
	if err != nil {
		if IsNotFoundErr(err) {
			glog.Infof("Instance %v not found", obj.Name)
			return nil
		}
		return err
	}

	glog.Infof("Starting DeleteInstance cloud operation")
	op, err := manager.instancesService.Delete(instanceURI(obj.Project, obj.Location, obj.Name)).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("DeleteInstance operation failed: %v", err)
	}

	err = manager.waitForOp(ctx, op)
	if err != nil {
		return fmt.Errorf("WaitFor DeleteInstance operation failed: %v", err)
	}

	instance, err = manager.GetInstance(ctx, obj)
	if err != nil && !IsNotFoundErr(err) {
		return fmt.Errorf("failed to get instance after deletion: %v", err)
	}
	if instance != nil {
		return fmt.Errorf("instance %v still exists after delete operation", obj.Name)
	}

	glog.Infof("Instance %v has been deleted", obj.Name)
	return nil
}

// ListInstances returns a list of active instances in a project at a specific location
func (manager *gcfsServiceManager) ListInstances(ctx context.Context, obj *ServiceInstance) ([]*ServiceInstance, error) {
	// Calling cloud provider service to get list of active instances. - indicates we are looking for instances in all the locations for a project
	instances, err := manager.instancesService.List(locationURI(obj.Project, "-")).Context(ctx).Do()
	if err != nil {
		return nil, err
	}

	var activeInstances []*ServiceInstance
	for _, activeInstance := range instances.Instances {
		serviceInstance, err := cloudInstanceToServiceInstance(activeInstance)
		if err != nil {
			return nil, err
		}
		activeInstances = append(activeInstances, serviceInstance)
	}
	return activeInstances, nil
}

func (manager *gcfsServiceManager) ResizeInstance(ctx context.Context, obj *ServiceInstance) (*ServiceInstance, error) {
	instanceuri := instanceURI(obj.Project, obj.Location, obj.Name)
	instance, err := manager.GetInstance(ctx, obj)
	if err != nil {
		glog.Errorf("Failed to get instance %s for resize operation, error %v", instanceuri, err)
		return nil, err
	}

	// High Scale tier supports shrink of capacity. However CSI spec does not support it.
	if util.BytesToGb(obj.Volume.SizeBytes) <= util.BytesToGb(instance.Volume.SizeBytes) {
		return instance, nil
	}

	// Create a file instance for the Patch request.
	betaObj := &filev1.Instance{
		Tier: obj.Tier,
		FileShares: []*filev1.FileShareConfig{
			{
				Name: instance.Volume.Name,
				// This is the updated instance size requested.
				CapacityGb: util.BytesToGb(obj.Volume.SizeBytes),
			},
		},
		Networks: []*filev1.NetworkConfig{
			{
				Network:         instance.Network.Name,
				Modes:           []string{"MODE_IPV4"},
				ReservedIpRange: instance.Network.ReservedIpRange,
			},
		},
	}

	glog.Infof("Starting Patch instance cloud operation for instance %s", instanceuri)
	glog.V(4).Infof("Patching instance %v: location %v, tier %v, capacity %v, network %v, ipRange %v",
		obj.Name,
		obj.Location,
		betaObj.Tier,
		betaObj.FileShares[0].CapacityGb,
		betaObj.Networks[0].Network,
		betaObj.Networks[0].ReservedIpRange)
	op, err := manager.instancesService.Patch(instanceuri, betaObj).UpdateMask(fileShareUpdateMask).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("Patch operation failed: %v", err)
	}

	err = manager.waitForOp(ctx, op)
	if err != nil {
		return nil, fmt.Errorf("WaitFor patch operation failed: %v", err)
	}

	instance, err = manager.GetInstance(ctx, obj)
	if err != nil {
		return nil, fmt.Errorf("failed to get instance after creation: %v", err)
	}
	glog.V(4).Infof("After resize got instance %#v", instance)
	return instance, nil
}

func (manager *gcfsServiceManager) GetBackup(ctx context.Context, backupUri string) (*BackupInfo, error) {
	backup, err := manager.backupService.Get(backupUri).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	return &BackupInfo{
		Backup:             backup,
		SourceVolumeHandle: backup.SourceInstance,
	}, nil
}

func (manager *gcfsServiceManager) CreateBackup(ctx context.Context, obj *ServiceInstance, backupName string, backupLocation string) (*filev1beta1.Backup, error) {
	backupUri, region, err := CreateBackpURI(obj, backupName, backupLocation)
	if err != nil {
		return nil, err
	}

	backupSource := fmt.Sprintf("projects/%s/locations/%s/instances/%s", obj.Project, obj.Location, obj.Name)
	backupobj := &filev1beta1.Backup{
		SourceInstance:  backupSource,
		SourceFileShare: obj.Volume.Name,
	}
	glog.V(4).Infof("Creating backup object %+v for the URI %v", *backupobj, backupUri)
	opbackup, err := manager.backupService.Create(locationURI(obj.Project, region), backupobj).BackupId(backupName).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("Create Backup operation failed: %v", err)
	}

	glog.V(4).Infof("Waiting for backup op %v to complete", opbackup.Name)
	err = manager.waitForV1beta1Op(ctx, opbackup)
	if err != nil {
		return nil, fmt.Errorf("WaitFor CreateBackup for source instance %v, backup Id: %v, operation failed: %v", backupSource, backupUri, err)
	}

	backupObj, err := manager.backupService.Get(backupUri).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	if backupObj.State != "READY" {
		return nil, fmt.Errorf("Backup %v for source %v is not ready, current state: %v", backupUri, backupSource, backupObj.State)
	}
	glog.Infof("Successfully created backup %+v for source instance %v", backupObj, backupSource)
	return backupObj, nil
}

func (manager *gcfsServiceManager) DeleteBackup(ctx context.Context, backupId string) error {
	_, err := manager.backupService.Get(backupId).Context(ctx).Do()
	if err != nil {
		if IsNotFoundErr(err) {
			glog.Infof("Backup %v not found", backupId)
			return nil
		}

		return err
	}

	opbackup, err := manager.backupService.Delete(backupId).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("Delete backup operation failed: %v", err)
	}

	glog.V(4).Infof("Waiting for backup op %v to complete", opbackup.Name)
	err = manager.waitForV1beta1Op(ctx, opbackup)
	if err != nil {
		return fmt.Errorf("Delete backup: %v, operation failed: %v", backupId, err)
	}

	glog.Infof("Backup %v successfully deleted", backupId)
	return nil
}

func (manager *gcfsServiceManager) waitForOp(ctx context.Context, op *filev1.Operation) error {
	return wait.Poll(5*time.Second, 5*time.Minute, func() (bool, error) {
		pollOp, err := manager.operationsService.Get(op.Name).Context(ctx).Do()
		if err != nil {
			return false, err
		}
		return isOpDone(pollOp)
	})
}

func isOpDone(op *filev1.Operation) (bool, error) {
	if op == nil {
		return false, nil
	}
	if op.Error != nil {
		return true, fmt.Errorf("operation %v failed (%v): %v", op.Name, op.Error.Code, op.Error.Message)
	}
	return op.Done, nil
}

func locationURI(project, location string) string {
	return fmt.Sprintf(locationURIFmt, project, location)
}

func instanceURI(project, location, name string) string {
	return fmt.Sprintf(instanceURIFmt, project, location, name)
}

func operationURI(project, location, name string) string {
	return fmt.Sprintf(operationURIFmt, project, location, name)
}

func backupURI(project, location, name string) string {
	return fmt.Sprintf(backupURIFmt, project, location, name)
}

func getInstanceNameFromURI(uri string) (project, location, name string, err error) {
	var uriRegex = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/instances/([^/]+)$`)

	substrings := uriRegex.FindStringSubmatch(uri)
	if substrings == nil {
		err = fmt.Errorf("failed to parse uri %v", uri)
		return
	}
	return substrings[1], substrings[2], substrings[3], nil
}

func IsNotFoundErr(err error) bool {
	apiErr, ok := err.(*googleapi.Error)
	if !ok {
		return false
	}

	for _, e := range apiErr.Errors {
		if e.Reason == "notFound" {
			return true
		}
	}
	return false
}

func (manager *gcfsServiceManager) waitForV1beta1Op(ctx context.Context, op *filev1beta1.Operation) error {
	return wait.Poll(5*time.Second, 5*time.Minute, func() (bool, error) {
		pollOp, err := manager.operationsV1beta1Service.Get(op.Name).Context(ctx).Do()
		if err != nil {
			return false, err
		}
		return isV1beta1OpDone(pollOp)
	})
}

func isV1beta1OpDone(op *filev1beta1.Operation) (bool, error) {
	if op == nil {
		return false, nil
	}
	if op.Error != nil {
		return true, fmt.Errorf("operation %v failed (%v): %v", op.Name, op.Error.Code, op.Error.Message)
	}
	return op.Done, nil
}

// This function returns the backup URI, the region that was picked to be the backup resource location and error.
func CreateBackpURI(obj *ServiceInstance, backupName, backupLocation string) (string, string, error) {
	region := backupLocation
	if region == "" {
		var err error
		region, err = util.GetRegionFromZone(obj.Location)
		if err != nil {
			return "", "", err
		}
	}

	return backupURI(obj.Project, region, backupName), region, nil
}
