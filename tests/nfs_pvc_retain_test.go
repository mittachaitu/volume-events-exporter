/*
Copyright © 2021 The MayaData Authors

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

package tests

import (
	"fmt"
	"time"

	"github.com/ghodss/yaml"
	"github.com/mayadata-io/volume-events-exporter/tests/nfs"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe("TEST NFS PVC WITH RETAIN RECLAIM POLICY", func() {
	var (
		// SC configuration
		backendSCName   = "openebs-hostpath"
		scNfsServerType = "kernel"

		// PVC configuration
		accessModes    = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}
		capacity       = "1Gi"
		pvcName        = "retain-nfs-pvc"
		scName         = "retain-openebs-rwx"
		nfsPVName      string
		backendPVCName string
		backendPVName  string

		// backend pvc configuration
		integrationTestFinalizer = "it.nfs.openebs.io/test-protection"

		maxRetryCount = 15
	)

	When("StorageClass with reclaim policy retain is created", func() {
		It("should create a StorageClass", func() {
			reclaimPolicy := corev1.PersistentVolumeReclaimRetain
			casObj := []Config{
				{
					Name:  KeyPVNFSServerType,
					Value: scNfsServerType,
				},
				{
					Name:  KeyPVBackendStorageClass,
					Value: backendSCName,
				},
			}
			casObjStr, err := yaml.Marshal(casObj)
			Expect(err).To(BeNil(), "while marshaling CAS object")

			err = Client.createStorageClass(&storagev1.StorageClass{
				ObjectMeta: metav1.ObjectMeta{
					Name: scName,
					Annotations: map[string]string{
						"openebs.io/cas-type":   "nfsrwx",
						"cas.openebs.io/config": string(casObjStr),
					},
				},
				Provisioner:   "openebs.io/nfsrwx",
				ReclaimPolicy: &reclaimPolicy,
			})
			Expect(err).To(BeNil(), "while creating SC {%s}", scName)
		})
	})

	When(fmt.Sprintf("pvc with storageclass %s is created", scName), func() {
		It("should create a pvc ", func() {
			By("creating above pvc")
			err := Client.createPVC(&corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      pvcName,
					Namespace: applicationNamespace,
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					StorageClassName: &scName,
					AccessModes:      accessModes,
					Resources: corev1.ResourceRequirements{
						Requests: map[corev1.ResourceName]resource.Quantity{
							corev1.ResourceStorage: resource.MustParse(capacity),
						},
					},
				},
			})
			Expect(err).To(BeNil(), "while creating pvc %s/%s", applicationNamespace, pvcName)

			pvcPhase, err := Client.waitForPVCBound(applicationNamespace, pvcName)
			Expect(err).To(BeNil(), "while waiting for pvc %s/%s bound phase", applicationNamespace, pvcName)
			Expect(pvcPhase).To(Equal(corev1.ClaimBound), "pvc %s/%s should be in bound phase", applicationNamespace, pvcName)

			err = markNFSResources(applicationNamespace, pvcName)
			Expect(err).To(BeNil(), "while makrking for events")
		})
	})

	When("pvc "+pvcName+"gets into bounded state", func() {
		It("should have NFS server in running state", func() {
			pvcObj, err := Client.getPVC(applicationNamespace, pvcName)
			Expect(err).To(BeNil(), "while fetching pvc %s/%s", applicationNamespace, pvcName)
			nfsPVName = pvcObj.Spec.VolumeName
			// NOTE: Backend PVC name will be nfs-<nfs pv name>
			backendPVCName = "nfs-" + nfsPVName

			nfsServerLabelSelector := "openebs.io/nfs-server=nfs-" + pvcObj.Spec.VolumeName
			err = Client.waitForPods(OpenEBSNamespace, nfsServerLabelSelector, corev1.PodRunning, 1)
			Expect(err).To(BeNil(), "while verifying NFS Server running status")
		})

		It("should have sent details to server... verifying annotation on backend PVC", func() {
			var backendPVCObj *corev1.PersistentVolumeClaim
			var err error
			var isEventReceived bool

			for retries := 0; retries < maxRetryCount; retries++ {
				backendPVCObj, err = Client.getPVC(OpenEBSNamespace, backendPVCName)
				Expect(err).To(BeNil(), "while fetching backend pvc %s/%s", OpenEBSNamespace, backendPVCName)

				_, isEventReceived = backendPVCObj.Annotations[nfs.VolumeCreateNFSPVCKey]
				if isEventReceived {
					break
				}
				// Reconciliation will happen at every 60 seconds but if any error occurs it will
				// get reconcile easily
				time.Sleep(time.Second * 10)
			}
			Expect(isEventReceived).To(BeTrue(), "NFS pvc %s/%s details are not exported to server", applicationNamespace, pvcName)
			backendPVC, err := Client.getPVC(OpenEBSNamespace, backendPVCName)
			Expect(err).To(BeNil(), "while fetching backend pvc %s/%s", OpenEBSNamespace, backendPVCName)
			backendPVName = backendPVC.Spec.VolumeName

			Expect(backendPVCObj.Annotations[nfs.VolumeCreateNFSPVCKey]).To(Equal(applicationNamespace+"-"+pvcName), "while verifying nfs pvc create event data")
			Expect(backendPVCObj.Annotations[nfs.VolumeCreateNFSPVKey]).To(Equal(nfsPVName), "while verifying nfs pv create event data")
			Expect(backendPVCObj.Annotations[nfs.VolumeCreateBackendPVCKey]).To(Equal(OpenEBSNamespace+"-"+backendPVCName), "while verifying backend pvc create event data")
			Expect(backendPVCObj.Annotations[nfs.VolumeCreateBackendPVKey]).To(Equal(backendPVName), "while verifying backend pv create event data")
		})
	})

	When("pvc "+pvcName+" with retain policy is deleted", func() {
		It("should delete pvc and pv should remain in same state", func() {
			err := Client.deletePVC(applicationNamespace, pvcName)
			Expect(err).To(BeNil(), "while deleting pvc %s/%s", applicationNamespace, pvcName)

			for retry := 5; retry >= 0; retry-- {
				nfsPVObj, err := Client.getPV(nfsPVName)
				Expect(err).To(BeNil(), "while fetching backend pv %s", nfsPVName)

				Expect(nfsPVObj.DeletionTimestamp).To(BeNil(), "NFS pv %s shouldn't be marked for deletion", nfsPVName)
				time.Sleep(time.Second * 5)
			}
		})
		It("should not send events to server", func() {
			var isDeleteEventReceived bool
			for retry := 5; retry >= 0; retry-- {
				backendPVCObj, err := Client.getPVC(OpenEBSNamespace, backendPVCName)
				Expect(err).To(BeNil(), "while fetching backend pvc %s/%s", OpenEBSNamespace, backendPVCName)

				_, isDeleteEventReceived = backendPVCObj.Annotations[nfs.VolumeDeleteNFSPVKey]
				if isDeleteEventReceived {
					break
				}
				time.Sleep(time.Second * 5)
			}
			Expect(isDeleteEventReceived).To(BeFalse(), "When NFS pvc %s/%s with retain policy is deleted it should not send events", applicationNamespace, pvcName)
		})
	})

	When("NFS pv "+nfsPVName+"is deleted ", func() {
		It("should delete nfs pv "+nfsPVName, func() {
			err := Client.deletePV(nfsPVName)
			Expect(err).To(BeNil(), "while deleting NFS pv %s", nfsPVName)

			var isNFSPVDeleted bool
			for retry := 0; retry < maxRetryCount; retry++ {
				_, err = Client.getPV(nfsPVName)
				if err != nil && k8serrors.IsNotFound(err) {
					isNFSPVDeleted = true
					break
				}
				Expect(err).To(BeNil(), "while fetching NFS pv %s", nfsPVName)
				time.Sleep(time.Second * 5)
			}
			Expect(isNFSPVDeleted).To(BeTrue(), "NFS pv %s should be deleted", nfsPVName)
		})

		It("should send events to server", func() {
			var backendPVCObj *corev1.PersistentVolumeClaim
			var err error
			var isEventReceived bool

			for retry := 0; retry < maxRetryCount; retry++ {
				backendPVCObj, err = Client.getPVC(OpenEBSNamespace, backendPVCName)
				Expect(err).To(BeNil(), "while fetching backend pvc %s/%s", OpenEBSNamespace, backendPVCName)

				_, isEventReceived = backendPVCObj.Annotations[nfs.VolumeDeleteNFSPVKey]
				if isEventReceived {
					break
				}
				// Reconciliation will happen at every 60 seconds but if any error occurs it will
				// get reconcile easily
				time.Sleep(time.Second * 10)
			}
			Expect(isEventReceived).To(BeTrue(), "NFS pv %s details are not exported to server for delete event", nfsPVName)
			Expect(backendPVCObj.Annotations[nfs.VolumeDeleteNFSPVKey]).To(Equal(nfsPVName), "while verifying NFS pv delete event data")
			Expect(backendPVCObj.Annotations[nfs.VolumeDeleteBackendPVCKey]).To(Equal(OpenEBSNamespace+"-"+backendPVCName), "while verifying backend pvc delete event data")
			Expect(backendPVCObj.Annotations[nfs.VolumeDeleteBackendPVKey]).To(Equal(backendPVName), "while verifying backend pv delete event data")
		})
	})

	When("test event finalizers are removed on resource", func() {
		It("should delete NFS related resources", func() {
			// Remove test finalizer on Backend PVC
			backendPVC, err := Client.getPVC(OpenEBSNamespace, backendPVCName)
			Expect(err).To(BeNil(), "while fetching backend pvc %s/%s", OpenEBSNamespace, backendPVCName)
			removeFinalizer(&backendPVC.ObjectMeta, integrationTestFinalizer)
			_, err = Client.updatePVC(backendPVC)
			Expect(err).To(BeNil(), "while removing test protection finalizer on backend pvc %s/%s", OpenEBSNamespace, backendPVCName)

			// Check backend PVC existence
			// NOTE: Garbage collector will delete NFS Resources(It will run for every 5 minutes)... So good to wait for >5min
			var isBackendPVCExist bool = true
			for retry := 0; retry < maxRetryCount; retry++ {
				_, err := Client.getPVC(OpenEBSNamespace, backendPVCName)
				if err != nil && k8serrors.IsNotFound(err) {
					isBackendPVCExist = false
					break
				}
				Expect(err).To(BeNil(), "while checking for existence of backend pvc %s/%s", OpenEBSNamespace, backendPVCName)
				time.Sleep(time.Second * 30)
			}
			Expect(isBackendPVCExist).To(BeFalse(), "backend pvc %s/%s shouldn't exist in cluster", OpenEBSNamespace, backendPVCName)

			// Check backend PV existence
			var isBackendPVExist bool = true
			for retry := 0; retry < maxRetryCount; retry++ {
				_, err := Client.getPV(backendPVName)
				if err != nil && k8serrors.IsNotFound(err) {
					isBackendPVExist = false
					break
				}
				Expect(err).To(BeNil(), "while checking for existence of backend pv %s", backendPVName)
				time.Sleep(time.Second * 10)
			}
			Expect(isBackendPVExist).To(BeFalse(), "backend pv %s shouldn't exist in cluster", backendPVName)
		})
	})

	When("StorageClass "+scName+" is deleted", func() {
		It("should delete the SC", func() {
			By("deleting SC " + scName)
			err := Client.deleteStorageClass(scName)
			Expect(err).To(BeNil(), "while deleting sc %s", scName)
		})
	})
})
