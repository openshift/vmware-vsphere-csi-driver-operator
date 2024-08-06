package cnsmigration

import (
	"sync"

	v1 "k8s.io/api/core/v1"
	"k8s.io/utils/set"
)

type volumeHandle string

type inUseVolumeStore struct {
	// map of volume Handle with set of PVCs using the volumeHandle
	volumesInUse map[volumeHandle]*volumeUseInfo
	// map of pvc name to volumeHandle for easy lookup
	pvcToVolumeHandleMap map[string]volumeHandle
	sync.RWMutex
}

type volumeUseInfo struct {
	// map of pvcName with set of pods using the pvc
	podPVCInfo map[string]set.Set[string]
}

func (pvcInfo *volumeUseInfo) addPVCName(pvcName string) {
	pvcInfo.podPVCInfo[pvcName] = set.New[string]()
}

func (c *volumeUseInfo) addPod(pvcName, podName string) {
	if podSet, ok := c.podPVCInfo[pvcName]; ok {
		podSet.Insert(podName)
		c.podPVCInfo[pvcName] = podSet
	} else {
		c.podPVCInfo[pvcName] = set.New(podName)
	}
}

func (c *volumeUseInfo) removePod(pvcName, podName string) {
	if podSet, ok := c.podPVCInfo[pvcName]; ok {
		podSet.Delete(podName)
		c.podPVCInfo[pvcName] = podSet
	}
}

func (c *volumeUseInfo) podsAreUsingIt() (string, string, bool) {
	for pvcName, podSet := range c.podPVCInfo {
		if podSet.Len() > 0 {
			podList := podSet.UnsortedList()
			return pvcName, podList[0], true
		}
	}
	return "", "", false
}

func NewInUseStore(pvList []v1.PersistentVolume) *inUseVolumeStore {
	inUseStore := &inUseVolumeStore{
		volumesInUse:         map[volumeHandle]*volumeUseInfo{},
		pvcToVolumeHandleMap: map[string]volumeHandle{},
	}

	for _, pv := range pvList {
		csiSource := pv.Spec.CSI
		if csiSource != nil && csiSource.Driver == vSphereCSIDriverName {
			pvcName := makeUniqueName(pv.Spec.ClaimRef)

			vh := volumeHandle(csiSource.VolumeHandle)
			// store pvc to volumeHandle Map for easy lookup
			inUseStore.pvcToVolumeHandleMap[pvcName] = vh

			if pvcInfo, ok := inUseStore.volumesInUse[vh]; ok {
				pvcInfo.addPVCName(pvcName)
			} else {
				inUseStore.volumesInUse[vh] = &volumeUseInfo{
					podPVCInfo: map[string]set.Set[string]{
						pvcName: set.New[string](),
					},
				}
			}
		}
	}
	return inUseStore
}

// given a PVC return volumeHandle that is associated with the pvc
func (s *inUseVolumeStore) getVolumeHandleFromPVC(pvcName string) (volumeHandle, bool) {
	if vh, ok := s.pvcToVolumeHandleMap[pvcName]; ok {
		return vh, ok
	} else {
		return "", false
	}
}

func (s *inUseVolumeStore) addPodPVCWithHandle(vh volumeHandle, podName, pvcName string) {
	vsInfo := s.volumesInUse[vh]
	vsInfo.addPod(pvcName, podName)
}

func (s *inUseVolumeStore) removePodPVCWithHandle(vh volumeHandle, podName, pvcName string) {
	vsinfo := s.volumesInUse[vh]
	vsinfo.removePod(pvcName, podName)
}

func (s *inUseVolumeStore) addAllPods(pods []v1.Pod) {
	s.Lock()
	defer s.Unlock()
	for _, pod := range pods {
		s.addPod(pod)
	}
}

func (s *inUseVolumeStore) addPod(pod v1.Pod) {
	if isPodTerminated(pod) {
		return
	}
	podUniqueName := pod.Namespace + "/" + pod.Name
	// add all PVCs that the pod uses
	for _, volume := range pod.Spec.Volumes {
		if volume.PersistentVolumeClaim != nil {
			pvcNameUniqueName := pod.Namespace + "/" + volume.PersistentVolumeClaim.ClaimName
			vh, found := s.getVolumeHandleFromPVC(pvcNameUniqueName)
			if found {
				s.addPodPVCWithHandle(vh, podUniqueName, pvcNameUniqueName)
			}
		}
	}
}

func (s *inUseVolumeStore) removePod(pod *v1.Pod) {
	s.Lock()
	defer s.Unlock()

	podUniqueName := pod.Namespace + "/" + pod.Name

	// remove all PVCs that this pod was using
	for _, volume := range pod.Spec.Volumes {
		if volume.PersistentVolumeClaim != nil {
			pvcNameUniqueName := pod.Namespace + "/" + volume.PersistentVolumeClaim.ClaimName
			vh, found := s.getVolumeHandleFromPVC(pvcNameUniqueName)
			if found {
				s.removePodPVCWithHandle(vh, podUniqueName, pvcNameUniqueName)
			}
		}
	}
}

// check if volumeHandle is actually in use, if yes - return pvc, pod, bool value
func (s *inUseVolumeStore) volumeInUse(vh volumeHandle) (string, string, bool) {
	s.RLock()
	defer s.RUnlock()

	if vsInfo, ok := s.volumesInUse[vh]; ok {
		return vsInfo.podsAreUsingIt()
	}
	return "", "", false
}

// isPodTerminated checks if pod is terminated
func isPodTerminated(pod v1.Pod) bool {
	podStatus := pod.Status
	return podStatus.Phase == v1.PodFailed || podStatus.Phase == v1.PodSucceeded
}

func makeUniqueName(obj *v1.ObjectReference) string {
	return obj.Namespace + "/" + obj.Name
}
