/*
Copyright 2020 The Kubernetes Authors.

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

package e2e

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	cnstypes "github.com/vmware/govmomi/cns/types"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	fnodes "k8s.io/kubernetes/test/e2e/framework/node"
	fpod "k8s.io/kubernetes/test/e2e/framework/pod"
	fpv "k8s.io/kubernetes/test/e2e/framework/pv"
	fss "k8s.io/kubernetes/test/e2e/framework/statefulset"
)

var _ = ginkgo.Describe("[csi-guest] CnsVolumeMetadata Metadatasyncer", func() {
	f := framework.NewDefaultFramework("e2e-guest-cluster-cnsvolumemetadata")
	var (
		client            clientset.Interface
		namespace         string
		scParameters      map[string]string
		storagePolicyName string
		svcPVCName        string // PVC Name in the Supervisor Cluster
		labelKey          string
		labelValue        string
		gcClusterID       string
		pvcUID            string
		manifestPath      = "tests/e2e/testing-manifests/statefulset/nginx"
		pvclabelKey       string
		pvclabelValue     string
		pvlabelKey        string
		pvlabelValue      string
	)
	ginkgo.BeforeEach(func() {
		client = f.ClientSet
		namespace = getNamespaceToRunTests(f)
		nodeList, err := fnodes.GetReadySchedulableNodes(f.ClientSet)
		framework.ExpectNoError(err, "Unable to find ready and schedulable Node")
		storagePolicyName = GetAndExpectStringEnvVar(envStoragePolicyNameForSharedDatastores)
		if !(len(nodeList.Items) > 0) {
			framework.Failf("Unable to find ready and schedulable Node")
		}
		bootstrap()
		scParameters = make(map[string]string)
		storagePolicyName = GetAndExpectStringEnvVar(envStoragePolicyNameForSharedDatastores)
		labelKey = "app"
		labelValue = "e2e-labels"
		pvclabelKey = "pvcapp"
		pvclabelValue = "e2e-labels-pvc"
		pvlabelKey = "pvapp"
		pvlabelValue = "e2e-labels-pv"
	})

	/*
		Steps:
		Create a PVC using any replicated storage class from the SV.
		Wait for PVC to be in Bound phase
		Verify CnsVolumeMetadata CRD in SV is created
		Create a Pod with this PVC mounted as a volume
		Verify entityReference for this volume on CNS contains entries for PV/PVC/POD in GC and PVC in SV.
		Delete Pod
		Delete PVC
	*/
	ginkgo.It("Verify CnsVolumeMetadata's entityReference for the volume on CNS", func() {
		var sc *storagev1.StorageClass
		var pvc *v1.PersistentVolumeClaim
		var err error
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ginkgo.By("CNS_TEST: Running for GC setup")
		ginkgo.By("Creating Storage Class and PVC")
		scParameters[svStorageClassName] = storagePolicyName
		sc, pvc, err = createPVCAndStorageClass(client, namespace, nil, scParameters, "", nil, "", false, "")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		defer func() {
			err := client.StorageV1().StorageClasses().Delete(ctx, sc.Name, *metav1.NewDeleteOptions(0))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}()

		ginkgo.By(fmt.Sprintf("Waiting for claim %s to be in bound phase", pvc.Name))
		pvs, err := fpv.WaitForPVClaimBoundPhase(client, []*v1.PersistentVolumeClaim{pvc}, framework.ClaimProvisionTimeout)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(pvs).NotTo(gomega.BeEmpty())
		pv := pvs[0]
		volumeID := pv.Spec.CSI.VolumeHandle
		// svcPVCName refers to PVC Name in the supervisor cluster
		svcPVCName = volumeID
		volumeID = getVolumeIDFromSupervisorCluster(svcPVCName)
		gomega.Expect(volumeID).NotTo(gomega.BeEmpty())
		defer func() {
			err := fpv.DeletePersistentVolumeClaim(client, pvc.Name, namespace)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = e2eVSphere.waitForCNSVolumeToBeDeleted(pv.Spec.CSI.VolumeHandle)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}()

		ginkgo.By("Creating pod")
		pod, err := fpod.CreatePod(client, namespace, nil, []*v1.PersistentVolumeClaim{pvc}, false, "")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By(fmt.Sprintf("Verify volume: %s is attached to the node: %s", pv.Spec.CSI.VolumeHandle, pod.Spec.NodeName))
		var vmUUID string
		vmUUID, err = getVMUUIDFromNodeName(pod.Spec.NodeName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		isDiskAttached, err := e2eVSphere.isVolumeAttachedToVM(client, volumeID, vmUUID)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(isDiskAttached).To(gomega.BeTrue(), fmt.Sprintf("Volume is not attached to the node, %s", vmUUID))

		podUID := string(pod.UID)
		framework.Logf("Pod uuid : " + podUID)
		framework.Logf("PVC name in SV " + svcPVCName)
		pvcUID = string(pvc.GetUID())
		framework.Logf("PVC UUID in GC " + pvcUID)
		gcClusterID = strings.Replace(svcPVCName, pvcUID, "", -1)

		framework.Logf("gcClusterId " + gcClusterID)
		pvUID := string(pv.UID)
		framework.Logf("PV uuid " + pvUID)

		verifyEntityReferenceInCRDInSupervisor(ctx, f, pv.Spec.CSI.VolumeHandle, crdCNSVolumeMetadatas, crdVersion, crdGroup, true, pv.Spec.CSI.VolumeHandle, false, nil, false)
		verifyEntityReferenceInCRDInSupervisor(ctx, f, gcClusterID+podUID, crdCNSVolumeMetadatas, crdVersion, crdGroup, true, pv.Spec.CSI.VolumeHandle, false, nil, false)
		verifyEntityReferenceInCRDInSupervisor(ctx, f, gcClusterID+pvUID, crdCNSVolumeMetadatas, crdVersion, crdGroup, true, pv.Spec.CSI.VolumeHandle, false, nil, false)

		ginkgo.By("Deleting the pod")
		err = fpod.DeletePodWithWait(client, pod)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By("Verify volume is detached from the node")
		isDiskDetached, err := e2eVSphere.waitForVolumeDetachedFromNode(client, pv.Spec.CSI.VolumeHandle, pod.Spec.NodeName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(isDiskDetached).To(gomega.BeTrue(), fmt.Sprintf("Volume %q is not detached from the node %q", pv.Spec.CSI.VolumeHandle, pod.Spec.NodeName))
	})

	/*
		Steps:
		Create a PVC using any replicated storage class from the SV.
		Wait for PVC to be in Bound phase
		Verify entityReference for this volume on CNS contains entries for PV/PVC in GC and PVC in SV.
		Update PVC Labels
		Verify CnsVolumeMetadata CRD in SV is updated
		Wait for labels to be present in CNS
		Delete PVC Labels
		Verify CnsVolumeMetadata CRD in SV is updated
		Wait for labels to be deleted in CNS
		Delete PVC

	*/
	ginkgo.It("Validate PVC labels are updated/deleted on CNS", func() {
		var sc *storagev1.StorageClass
		var pvc *v1.PersistentVolumeClaim
		var err error
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ginkgo.By("CNS_TEST: Running for GC setup")
		ginkgo.By("Creating Storage Class and PVC")
		scParameters[svStorageClassName] = storagePolicyName
		sc, pvc, err = createPVCAndStorageClass(client, namespace, nil, scParameters, "", nil, "", false, "")

		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		defer func() {
			err := client.StorageV1().StorageClasses().Delete(ctx, sc.Name, *metav1.NewDeleteOptions(0))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}()

		ginkgo.By(fmt.Sprintf("Waiting for claim %s to be in bound phase", pvc.Name))
		pvs, err := fpv.WaitForPVClaimBoundPhase(client, []*v1.PersistentVolumeClaim{pvc}, framework.ClaimProvisionTimeout)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(pvs).NotTo(gomega.BeEmpty())
		pv := pvs[0]
		volumeID := pv.Spec.CSI.VolumeHandle
		// svcPVCName refers to PVC Name in the supervisor cluster
		svcPVCName = volumeID
		volumeID = getVolumeIDFromSupervisorCluster(svcPVCName)
		gomega.Expect(volumeID).NotTo(gomega.BeEmpty())
		defer func() {
			err := fpv.DeletePersistentVolumeClaim(client, pvc.Name, namespace)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = e2eVSphere.waitForCNSVolumeToBeDeleted(pv.Spec.CSI.VolumeHandle)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}()

		framework.Logf("PVC name in SV " + svcPVCName)
		pvcUID = string(pvc.GetUID())
		framework.Logf("PVC UUID in GC " + pvcUID)
		gcClusterID = strings.Replace(svcPVCName, pvcUID, "", -1)

		framework.Logf("gcClusterId " + gcClusterID)
		pvUID := string(pv.UID)
		framework.Logf("PV uuid " + pvUID)

		verifyEntityReferenceInCRDInSupervisor(ctx, f, pv.Spec.CSI.VolumeHandle, crdCNSVolumeMetadatas, crdVersion, crdGroup, true, pv.Spec.CSI.VolumeHandle, false, nil, false)
		verifyEntityReferenceInCRDInSupervisor(ctx, f, gcClusterID+pvUID, crdCNSVolumeMetadatas, crdVersion, crdGroup, true, pv.Spec.CSI.VolumeHandle, false, nil, false)

		labels := make(map[string]string)
		labels[labelKey] = labelValue

		ginkgo.By(fmt.Sprintf("Updating labels %+v for pvc %s in namespace %s", labels, pvc.Name, pvc.Namespace))
		pvc, err = client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvc.Name, metav1.GetOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		pvc.Labels = labels
		_, err = client.CoreV1().PersistentVolumeClaims(namespace).Update(ctx, pvc, metav1.UpdateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// TODO: replace sleep with polling mechanism
		framework.Logf("Sleeping for 20 seconds for the labels to be updated")
		time.Sleep(20 * time.Second)

		verifyEntityReferenceInCRDInSupervisor(ctx, f, pv.Spec.CSI.VolumeHandle, crdCNSVolumeMetadatas, crdVersion, crdGroup, true, pv.Spec.CSI.VolumeHandle, true, labels, true)

	})

	/*
		Steps:
		Create a PVC using any replicated storage class from the SV.
		Wait for PVC to be in Bound phase
		Create a Pod attached to above PV
		Verify CnsVolumeMetadata CRD in SV is created
		Wait for Pod name to be present in CNS
		Verify entityReference for this volume on CNS contains entries for PV/PVC/POD in GC and PVC in SV.
		Delete Pod and wait for disk to be detached
		Verify CnsVolumeMetadata CRD in SV is deleted
		Wait for Pod name to be deleted in CNS
		Delete PVC
	*/
	ginkgo.It("Verify Pod Name is updated/deleted on CNS", func() {
		var sc *storagev1.StorageClass
		var pvc *v1.PersistentVolumeClaim
		var err error
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ginkgo.By("CNS_TEST: Running for GC setup")
		ginkgo.By("Creating Storage Class and PVC")
		scParameters[svStorageClassName] = storagePolicyName
		sc, pvc, err = createPVCAndStorageClass(client, namespace, nil, scParameters, "", nil, "", false, "")

		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		defer func() {
			err := client.StorageV1().StorageClasses().Delete(ctx, sc.Name, *metav1.NewDeleteOptions(0))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}()

		ginkgo.By(fmt.Sprintf("Waiting for claim %s to be in bound phase", pvc.Name))
		pvs, err := fpv.WaitForPVClaimBoundPhase(client, []*v1.PersistentVolumeClaim{pvc}, framework.ClaimProvisionTimeout)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(pvs).NotTo(gomega.BeEmpty())
		pv := pvs[0]
		volumeID := pv.Spec.CSI.VolumeHandle
		// svcPVCName refers to PVC Name in the supervisor cluster
		svcPVCName = volumeID
		volumeID = getVolumeIDFromSupervisorCluster(svcPVCName)
		gomega.Expect(volumeID).NotTo(gomega.BeEmpty())
		defer func() {
			err := fpv.DeletePersistentVolumeClaim(client, pvc.Name, namespace)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = e2eVSphere.waitForCNSVolumeToBeDeleted(pv.Spec.CSI.VolumeHandle)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}()

		ginkgo.By("Creating pod")
		pod, err := fpod.CreatePod(client, namespace, nil, []*v1.PersistentVolumeClaim{pvc}, false, "")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By(fmt.Sprintf("Verify volume: %s is attached to the node: %s", pv.Spec.CSI.VolumeHandle, pod.Spec.NodeName))
		var vmUUID string
		vmUUID, err = getVMUUIDFromNodeName(pod.Spec.NodeName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		isDiskAttached, err := e2eVSphere.isVolumeAttachedToVM(client, volumeID, vmUUID)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(isDiskAttached).To(gomega.BeTrue(), fmt.Sprintf("Volume is not attached to the node, %s", vmUUID))

		podUID := string(pod.UID)
		framework.Logf("Pod uuid : " + podUID)
		framework.Logf("PVC name in SV " + svcPVCName)
		pvcUID = string(pvc.GetUID())
		framework.Logf("PVC UUID in GC " + pvcUID)
		gcClusterID = strings.Replace(svcPVCName, pvcUID, "", -1)

		framework.Logf("gcClusterId " + gcClusterID)
		pvUID := string(pv.UID)
		framework.Logf("PV uuid " + pvUID)

		verifyEntityReferenceInCRDInSupervisor(ctx, f, pv.Spec.CSI.VolumeHandle, crdCNSVolumeMetadatas, crdVersion, crdGroup, true, pv.Spec.CSI.VolumeHandle, false, nil, false)
		verifyEntityReferenceInCRDInSupervisor(ctx, f, gcClusterID+podUID, crdCNSVolumeMetadatas, crdVersion, crdGroup, true, pv.Spec.CSI.VolumeHandle, false, nil, false)
		verifyEntityReferenceInCRDInSupervisor(ctx, f, gcClusterID+pvUID, crdCNSVolumeMetadatas, crdVersion, crdGroup, true, pv.Spec.CSI.VolumeHandle, false, nil, false)

		ginkgo.By("Deleting the pod")
		err = fpod.DeletePodWithWait(client, pod)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By("Verify volume is detached from the node")
		isDiskDetached, err := e2eVSphere.waitForVolumeDetachedFromNode(client, pv.Spec.CSI.VolumeHandle, pod.Spec.NodeName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(isDiskDetached).To(gomega.BeTrue(), fmt.Sprintf("Volume %q is not detached from the node %q", pv.Spec.CSI.VolumeHandle, pod.Spec.NodeName))

		// TODO: replace sleep with polling mechanism
		ginkgo.By("Sleeping for 20s for update...")
		time.Sleep(20 * time.Second)
		//Verifying the  CnsVolumeMetadata CRD  for Pod in SV is deleted
		verifyEntityReferenceInCRDInSupervisor(ctx, f, gcClusterID+podUID, crdCNSVolumeMetadatas, crdVersion, crdGroup, false, pv.Spec.CSI.VolumeHandle, false, nil, false)
	})

	/*
		Steps:
		1. Create a Storage Class
		2. Create a statefulset with 3 replicas
		3. Wait for all PVCs to be in Bound phase and Pods are Ready state
		4. Update PVC labels
		5. Verify PVC labels are updated on CNS
		6. Scale up number of replicas to 5
		7. Update PV labels
		8. Verify PV labels are updated on CNS
		9. Scale down statefulsets to 0 replicas and delete all pods.
		10. Delete PVCs
		11. Delete SC
	*/

	ginkgo.It("Statefulset tests with label updates", func() {
		var sc *storagev1.StorageClass
		var err error
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ginkgo.By("CNS_TEST: Running for GC setup")

		ginkgo.By("Creating StorageClass for Statefulset")
		scParameters[svStorageClassName] = storagePolicyName
		scSpec := getVSphereStorageClassSpec(storageclassname, scParameters, nil, "", "", false)
		sc, err = client.StorageV1().StorageClasses().Create(ctx, scSpec, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		defer func() {
			err := client.StorageV1().StorageClasses().Delete(ctx, sc.Name, *metav1.NewDeleteOptions(0))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}()

		ginkgo.By("Creating statefulset")
		statefulset := fss.CreateStatefulSet(client, manifestPath, namespace)
		defer func() {
			ginkgo.By(fmt.Sprintf("Deleting all statefulsets in namespace: %v", namespace))
			fss.DeleteAllStatefulSets(client, namespace)
			if supervisorCluster {
				ginkgo.By(fmt.Sprintf("Deleting service nginx in namespace: %v", namespace))
				err := client.CoreV1().Services(namespace).Delete(ctx, servicename, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			}
		}()
		replicas := *(statefulset.Spec.Replicas)
		// Waiting for pods status to be Ready
		fss.WaitForStatusReadyReplicas(client, statefulset, replicas)
		gomega.Expect(fss.CheckMount(client, statefulset, mountPath)).NotTo(gomega.HaveOccurred())
		ssPodsBeforeScaleup := fss.GetPodList(client, statefulset)
		gomega.Expect(ssPodsBeforeScaleup.Items).NotTo(gomega.BeEmpty(), fmt.Sprintf("Unable to get list of Pods from the Statefulset: %v", statefulset.Name))
		gomega.Expect(len(ssPodsBeforeScaleup.Items) == int(replicas)).To(gomega.BeTrue(), "Number of Pods in the statefulset should match with number of replicas")

		pvclabels := make(map[string]string)
		pvclabels[pvclabelKey] = pvclabelValue
		var volumeID string

		for _, sspod := range ssPodsBeforeScaleup.Items {
			_, err := client.CoreV1().Pods(namespace).Get(ctx, sspod.Name, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			for _, volumespec := range sspod.Spec.Volumes {
				if volumespec.PersistentVolumeClaim != nil {
					pv := getPvFromClaim(client, statefulset.Namespace, volumespec.PersistentVolumeClaim.ClaimName)
					ginkgo.By(fmt.Sprintf("Updating labels %+v for pvc %s in namespace %s", pvclabels, volumespec.PersistentVolumeClaim.ClaimName, namespace))
					pvc, err := client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, volumespec.PersistentVolumeClaim.ClaimName, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					pvc.Labels = pvclabels
					_, err = client.CoreV1().PersistentVolumeClaims(namespace).Update(ctx, pvc, metav1.UpdateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					volumeID = getVolumeIDFromSupervisorCluster(pv.Spec.CSI.VolumeHandle)
					gomega.Expect(volumeID).NotTo(gomega.BeEmpty())
					framework.Logf("value of volumeID " + volumeID)
					ginkgo.By(fmt.Sprintf("Waiting for labels %+v to be updated for pvc %s in namespace %s", pvclabels, volumespec.PersistentVolumeClaim.ClaimName, GetAndExpectStringEnvVar(envSupervisorClusterNamespace)))
					err = e2eVSphere.waitForLabelsToBeUpdated(volumeID, pvclabels, string(cnstypes.CnsKubernetesEntityTypePVC), volumespec.PersistentVolumeClaim.ClaimName, namespace)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				}
			}
		}

		ginkgo.By(fmt.Sprintf("Scaling up statefulsets to number of Replica: %v", replicas+2))
		_, scaleupErr := fss.Scale(client, statefulset, replicas+2)
		gomega.Expect(scaleupErr).NotTo(gomega.HaveOccurred())
		fss.WaitForStatusReplicas(client, statefulset, replicas+2)
		fss.WaitForStatusReadyReplicas(client, statefulset, replicas+2)
		pvlabels := make(map[string]string)
		pvlabels[pvlabelKey] = pvlabelValue

		ssPodsAfterScaleUp := fss.GetPodList(client, statefulset)

		for _, spod := range ssPodsAfterScaleUp.Items {
			_, err := client.CoreV1().Pods(namespace).Get(ctx, spod.Name, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			for _, volumespec := range spod.Spec.Volumes {
				if volumespec.PersistentVolumeClaim != nil {
					pv := getPvFromClaim(client, statefulset.Namespace, volumespec.PersistentVolumeClaim.ClaimName)
					ginkgo.By(fmt.Sprintf("Updating labels %+v for pv %s", pvlabels, pv.Name))
					pv.Labels = pvlabels
					pv, err = client.CoreV1().PersistentVolumes().Update(ctx, pv, metav1.UpdateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					volumeID = getVolumeIDFromSupervisorCluster(pv.Spec.CSI.VolumeHandle)
					gomega.Expect(volumeID).NotTo(gomega.BeEmpty())
					framework.Logf("value of volumeID " + volumeID)
					ginkgo.By(fmt.Sprintf("Waiting for labels %+v to be updated for pv %s", pvlabels, pv.Name))
					err = e2eVSphere.waitForLabelsToBeUpdated(volumeID, pvlabels, string(cnstypes.CnsKubernetesEntityTypePV), pv.Name, "")
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				}
			}
		}

		ginkgo.By(fmt.Sprintf("Scaling down statefulsets to number of Replica: %v", 0))
		_, scaledownErr := fss.Scale(client, statefulset, 0)
		gomega.Expect(scaledownErr).NotTo(gomega.HaveOccurred())
		fss.WaitForStatusReadyReplicas(client, statefulset, 0)
		ssPodsAfterScaleDown := fss.GetPodList(client, statefulset)
		gomega.Expect(len(ssPodsAfterScaleDown.Items) == int(0)).To(gomega.BeTrue(), "Number of Pods in the statefulset should match with number of replicas")
	})

	/*
		Steps:
		1.Create a PVC using any replicated storage class from the SV.
		2.Wait for PVC to be in Bound phase
		3.Bring down csi-controller pod in SV
		4.Update PV/PVC labels
		5.Verify CnsVolumeMetadata CRDs are updated.
		6.Bring up csi-controller pod in SV
		7.Verify PV and PVC entry is updated in CNS
		8.Delete PVC
	*/
	ginkgo.It("Verify CNS Operator receives callbacks on all objects when csi-controller was brought back up", func() {
		var sc *storagev1.StorageClass
		var pvc *v1.PersistentVolumeClaim
		var err error
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ginkgo.By("CNS_TEST: Running for GC setup")
		ginkgo.By("Creating Storage Class and PVC")
		scParameters[svStorageClassName] = storagePolicyName
		sc, pvc, err = createPVCAndStorageClass(client, namespace, nil, scParameters, "", nil, "", false, "")

		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		defer func() {
			err := client.StorageV1().StorageClasses().Delete(ctx, sc.Name, *metav1.NewDeleteOptions(0))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}()

		ginkgo.By(fmt.Sprintf("Waiting for claim %s to be in bound phase", pvc.Name))
		pvs, err := fpv.WaitForPVClaimBoundPhase(client, []*v1.PersistentVolumeClaim{pvc}, framework.ClaimProvisionTimeout)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(pvs).NotTo(gomega.BeEmpty())
		pv := pvs[0]
		volumeID := pv.Spec.CSI.VolumeHandle
		// svcPVCName refers to PVC Name in the supervisor cluster
		svcPVCName = volumeID
		volumeID = getVolumeIDFromSupervisorCluster(svcPVCName)
		gomega.Expect(volumeID).NotTo(gomega.BeEmpty())
		defer func() {
			err := fpv.DeletePersistentVolumeClaim(client, pvc.Name, namespace)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = e2eVSphere.waitForCNSVolumeToBeDeleted(pv.Spec.CSI.VolumeHandle)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}()

		ginkgo.By("Creating pod")
		pod, err := fpod.CreatePod(client, namespace, nil, []*v1.PersistentVolumeClaim{pvc}, false, "")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By(fmt.Sprintf("Verify volume: %s is attached to the node: %s", pv.Spec.CSI.VolumeHandle, pod.Spec.NodeName))
		var vmUUID string
		vmUUID, err = getVMUUIDFromNodeName(pod.Spec.NodeName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		isDiskAttached, err := e2eVSphere.isVolumeAttachedToVM(client, volumeID, vmUUID)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(isDiskAttached).To(gomega.BeTrue(), fmt.Sprintf("Volume is not attached to the node, %s", vmUUID))

		podUID := string(pod.UID)
		framework.Logf("Pod uuid : " + podUID)
		framework.Logf("PVC name in SV " + svcPVCName)
		pvcUID = string(pvc.GetUID())
		framework.Logf("PVC UUID in GC " + pvcUID)
		gcClusterID = strings.Replace(svcPVCName, pvcUID, "", -1)

		framework.Logf("gcClusterId " + gcClusterID)
		pvUID := string(pv.UID)
		framework.Logf("PV uuid " + pvUID)

		verifyEntityReferenceInCRDInSupervisor(ctx, f, pv.Spec.CSI.VolumeHandle, crdCNSVolumeMetadatas, crdVersion, crdGroup, true, pv.Spec.CSI.VolumeHandle, false, nil, false)
		verifyEntityReferenceInCRDInSupervisor(ctx, f, gcClusterID+podUID, crdCNSVolumeMetadatas, crdVersion, crdGroup, true, pv.Spec.CSI.VolumeHandle, false, nil, false)
		verifyEntityReferenceInCRDInSupervisor(ctx, f, gcClusterID+pvUID, crdCNSVolumeMetadatas, crdVersion, crdGroup, true, pv.Spec.CSI.VolumeHandle, false, nil, false)

		ginkgo.By("Scaling down the csi driver to zero replica")
		deployment := updateDeploymentReplica(client, 0, vSphereCSIControllerPodNamePrefix, csiSystemNamespace)
		ginkgo.By(fmt.Sprintf("Successfully scaled down the csi driver deployment:%s to zero replicas", deployment.Name))

		labels := make(map[string]string)
		labels[labelKey] = labelValue

		ginkgo.By(fmt.Sprintf("Updating labels %+v for pvc %s in namespace %s", labels, pvc.Name, pvc.Namespace))
		pvc, err = client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvc.Name, metav1.GetOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		pvc.Labels = labels
		_, err = client.CoreV1().PersistentVolumeClaims(namespace).Update(ctx, pvc, metav1.UpdateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		verifyEntityReferenceInCRDInSupervisor(ctx, f, pv.Spec.CSI.VolumeHandle, crdCNSVolumeMetadatas, crdVersion, crdGroup, true, pv.Spec.CSI.VolumeHandle, false, nil, false)

		ginkgo.By("Scaling up the csi driver to one replica")
		deployment = updateDeploymentReplica(client, 1, vSphereCSIControllerPodNamePrefix, csiSystemNamespace)
		ginkgo.By(fmt.Sprintf("Successfully scaled up the csi driver deployment:%s to one replica", deployment.Name))

		// TODO: replace sleep with polling mechanism
		framework.Logf("Sleeping for 60 seconds")
		time.Sleep(60 * time.Second)
		verifyEntityReferenceInCRDInSupervisor(ctx, f, pv.Spec.CSI.VolumeHandle, crdCNSVolumeMetadatas, crdVersion, crdGroup, true, pv.Spec.CSI.VolumeHandle, true, labels, true)

		ginkgo.By("Deleting the pod")
		err = fpod.DeletePodWithWait(client, pod)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By("Verify volume is detached from the node")
		isDiskDetached, err := e2eVSphere.waitForVolumeDetachedFromNode(client, pv.Spec.CSI.VolumeHandle, pod.Spec.NodeName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(isDiskDetached).To(gomega.BeTrue(), fmt.Sprintf("Volume %q is not detached from the node %q", pv.Spec.CSI.VolumeHandle, pod.Spec.NodeName))
	})

})
