package syncer

import (
	"context"

	"k8s.io/client-go/tools/cache"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"

	cnstypes "github.com/vmware/govmomi/cns/types"
	volumes "sigs.k8s.io/vsphere-csi-driver/pkg/common/cns-lib/volume"
	"sigs.k8s.io/vsphere-csi-driver/pkg/csi/service/common"
	"sigs.k8s.io/vsphere-csi-driver/pkg/csi/service/logger"
	csitypes "sigs.k8s.io/vsphere-csi-driver/pkg/csi/types"
)

// getPVsInBoundAvailableOrReleased return PVs in Bound, Available or Released state
func getPVsInBoundAvailableOrReleased(ctx context.Context, metadataSyncer *metadataSyncInformer) ([]*v1.PersistentVolume, error) {
	log := logger.GetLogger(ctx)
	var pvsInDesiredState []*v1.PersistentVolume
	log.Debugf("FullSync: Getting all PVs in Bound, Available or Released state")
	// Get all PVs from kubernetes
	allPVs, err := metadataSyncer.pvLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}
	for _, pv := range allPVs {
		if pv.Spec.CSI != nil && pv.Spec.CSI.Driver == csitypes.Name {
			log.Debugf("FullSync: pv %v is in state %v", pv.Spec.CSI.VolumeHandle, pv.Status.Phase)
			if pv.Status.Phase == v1.VolumeBound || pv.Status.Phase == v1.VolumeAvailable || pv.Status.Phase == v1.VolumeReleased {
				pvsInDesiredState = append(pvsInDesiredState, pv)
			}
		}
	}
	return pvsInDesiredState, nil
}

// getBoundPVs return PVs in Bound state
func getBoundPVs(ctx context.Context, metadataSyncer *metadataSyncInformer) ([]*v1.PersistentVolume, error) {
	log := logger.GetLogger(ctx)
	var boundPVs []*v1.PersistentVolume
	// Get all PVs from kubernetes
	allPVs, err := metadataSyncer.pvLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}
	for _, pv := range allPVs {
		if pv.Spec.CSI != nil && pv.Spec.CSI.Driver == csitypes.Name {
			log.Debugf("getBoundPVs: pv %s with volumeHandle %s is in state %v", pv.Name, pv.Spec.CSI.VolumeHandle, pv.Status.Phase)
			if pv.Status.Phase == v1.VolumeBound {
				boundPVs = append(boundPVs, pv)
			}
		}
	}
	return boundPVs, nil
}

// IsValidVolume determines if the given volume mounted by a POD is a valid vsphere volume. Returns the pv and pvc object if true.
func IsValidVolume(ctx context.Context, volume v1.Volume, pod *v1.Pod, metadataSyncer *metadataSyncInformer) (bool, *v1.PersistentVolume, *v1.PersistentVolumeClaim) {
	log := logger.GetLogger(ctx)
	pvcName := volume.PersistentVolumeClaim.ClaimName
	// Get pvc attached to pod
	pvc, err := metadataSyncer.pvcLister.PersistentVolumeClaims(pod.Namespace).Get(pvcName)
	if err != nil {
		log.Errorf("Error getting Persistent Volume Claim for volume %s with err: %v", volume.Name, err)
		return false, nil, nil
	}

	// Get pv object attached to pvc
	pv, err := metadataSyncer.pvLister.Get(pvc.Spec.VolumeName)
	if err != nil {
		log.Errorf("Error getting Persistent Volume for PVC %s in volume %s with err: %v", pvc.Name, volume.Name, err)
		return false, nil, nil
	}

	// Verify if pv is vsphere csi volume
	if (pv.Spec.CSI == nil || pv.Spec.CSI.Driver != csitypes.Name) && (metadataSyncer.configInfo.Cfg.FeatureStates.CSIMigration && pv.Spec.VsphereVolume == nil) {
		return false, nil, nil
	}
	//Verify if pv is vsphere volume and migration flag is disabled
	if pv.Spec.VsphereVolume != nil && !metadataSyncer.configInfo.Cfg.FeatureStates.CSIMigration {
		log.Warnf("PVCUpdated: volume-migration feature switch is disabled. Cannot update vSphere volume metadata %s for the pod %s", pv.Name, pod.Name)
		return false, nil, nil
	}
	return true, pv, pvc
}

// getQueryResults returns list of CnsQueryResult retrieved using
// queryFilter with offset and limit to query volumes using pagination
// if volumeIds is empty, then all volumes from CNS will be retrieved by pagination
func getQueryResults(ctx context.Context, volumeIds []cnstypes.CnsVolumeId, clusterID string, volumeManager volumes.Manager) ([]*cnstypes.CnsQueryResult, error) {
	log := logger.GetLogger(ctx)
	queryFilter := cnstypes.CnsQueryFilter{
		VolumeIds: volumeIds,
		Cursor: &cnstypes.CnsCursor{
			Offset: 0,
			Limit:  queryVolumeLimit,
		},
	}
	if clusterID != "" {
		queryFilter.ContainerClusterIds = []string{clusterID}
	}
	var allQueryResults []*cnstypes.CnsQueryResult
	for {
		log.Debugf("Query volumes with offset: %v and limit: %v", queryFilter.Cursor.Offset, queryFilter.Cursor.Limit)
		queryResult, err := volumeManager.QueryVolume(ctx, queryFilter)
		if err != nil {
			log.Errorf("failed to QueryVolume using filter: %+v", queryFilter)
			return nil, err
		}
		if queryResult == nil {
			log.Info("Observed empty queryResult")
			break
		}
		allQueryResults = append(allQueryResults, queryResult)
		log.Infof("%v more volumes to be queried", queryResult.Cursor.TotalRecords-queryResult.Cursor.Offset)
		if queryResult.Cursor.Offset == queryResult.Cursor.TotalRecords {
			log.Info("Metadata retrieved for all requested volumes")
			break
		}
		queryFilter.Cursor = &queryResult.Cursor
	}
	return allQueryResults, nil
}

// getPVCKey helps to get the PVC name from PVC object
func getPVCKey(ctx context.Context, obj interface{}) (string, error) {
	log := logger.GetLogger(ctx)

	if unknown, ok := obj.(cache.DeletedFinalStateUnknown); ok && unknown.Obj != nil {
		obj = unknown.Obj
	}
	objKey, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		log.Errorf("Failed to get key from object: %v", err)
		return "", err
	}
	log.Infof("getPVCKey: PVC key %s", objKey)
	return objKey, nil
}

// HasMigratedToAnnotation returns true if the migrated-to annotation is found in the newer object
func HasMigratedToAnnotation(ctx context.Context, prevAnnotations map[string]string, newAnnotations map[string]string) bool {
	log := logger.GetLogger(ctx)
	// Checking if the migrated-to annotation is found in the new PV
	if _, annMigratedToFound := newAnnotations[common.AnnMigratedTo]; annMigratedToFound {
		if _, annMigratedToFound = prevAnnotations[common.AnnMigratedTo]; !annMigratedToFound {
			log.Debugf("Received %v annotation update", common.AnnMigratedTo)
			return true
		}
	}
	log.Debug("%v annotation not found", common.AnnMigratedTo)
	return false
}
