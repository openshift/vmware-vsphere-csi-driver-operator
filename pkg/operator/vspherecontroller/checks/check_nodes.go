package checks

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

const (
	nodeCheckTimeout          = 5 * time.Minute
	hardwareVersionPrefix     = "vmx-"
	minHardwareVersion        = 15
	minRequiredHostVersion    = "6.7.3"
	minUpgradeableHostVersion = "7.0.2"
	workerCount               = 10
)

var (
	nodeProperties = []string{"config.extraConfig", "config.flags", "config.version", "runtime.host"}
)

type nodeChannelWorkData struct {
	checkOpts CheckArgs
	node      *v1.Node
	ctx       context.Context
}

type NodeChecker struct {
	hostESXIVersions map[string]bool
	wg               sync.WaitGroup
	esxiVersionLock  sync.RWMutex

	results     []ClusterCheckResult
	resultLock  sync.RWMutex
	workChannel chan nodeChannelWorkData
}

var _ CheckInterface = &NodeChecker{}

func (n *NodeChecker) createPool(workerCount int) {
	n.workChannel = make(chan nodeChannelWorkData, workerCount)

	for i := 0; i < workerCount; i++ {
		routineIndex := i
		go n.runWorker(routineIndex)
		n.wg.Add(1)
	}
}

func (n *NodeChecker) waitForWorkFinish() []ClusterCheckResult {
	n.wg.Wait()
	// At this point no goroutine should be accessing results anyways
	if n.getResultCount() > 0 {
		return n.results
	} else {
		return []ClusterCheckResult{MakeClusterCheckResultPass()}
	}
}

// add work to workqueue but if one or more of workers have already submitted a failed check result
// return false so as we can stop processing more nodes
func (n *NodeChecker) addWork(work nodeChannelWorkData) bool {
	if n.getResultCount() > 0 {
		return false
	}
	n.workChannel <- work
	return true
}

func (n *NodeChecker) runWorker(workIndex int) {
	for {
		workData, ok := <-n.workChannel
		if !ok {
			break
		}
		response := n.checkOnNode(workData)

		// if there was an error performing node check we can add result to worker results
		// this will allow checks to fail quickly.
		if response.CheckError != nil {
			n.addResult(response)
			break
		}
	}
	n.wg.Done()
}

func (n *NodeChecker) addResult(res ClusterCheckResult) {
	n.resultLock.Lock()
	defer n.resultLock.Unlock()
	n.results = append(n.results, res)
}

func (n *NodeChecker) getResultCount() int {
	n.resultLock.RLock()
	defer n.resultLock.RUnlock()
	return len(n.results)
}

func (n *NodeChecker) checkOnNode(workInfo nodeChannelWorkData) ClusterCheckResult {
	checkOpts := workInfo.checkOpts
	node := workInfo.node

	nodeCheckContext, cancel := context.WithTimeout(workInfo.ctx, nodeCheckTimeout)
	defer cancel()

	vm, err := getVM(nodeCheckContext, checkOpts, node)
	if err != nil {
		return makeDeprecatedEnvironmentError(CheckStatusVcenterAPIError, err)
	}

	hwVersion := vm.Config.Version
	vmHWVersion := strings.Trim(hwVersion, hardwareVersionPrefix)
	versionInt, err := strconv.ParseInt(vmHWVersion, 0, 64)
	if err != nil {
		klog.Errorf("error parsing hardware version %s: %v", hwVersion, err)
		return makeDeprecatedEnvironmentError(CheckStatusVcenterAPIError, err)
	}

	if versionInt < minHardwareVersion {
		reason := fmt.Errorf("node %s has hardware version %s, which is below the minimum required version %d", node.Name, hwVersion, minHardwareVersion)
		return makeDeprecatedEnvironmentError(CheckStatusDeprecatedHWVersion, reason)
	}

	// check for esxi version
	hostRef := vm.Runtime.Host
	if hostRef == nil {
		klog.Errorf("error getting ESXI host for node %s: vm.runtime.host is empty", node.Name)
		return makeDeprecatedEnvironmentError(CheckStatusVcenterAPIError, err)
	}

	hostName := hostRef.Value
	if beingProcessed := n.checkOrMarkHostForProcessing(hostName); beingProcessed {
		return MakeClusterCheckResultPass()
	}

	hostSystem, err := n.getHost(nodeCheckContext, checkOpts, hostRef)
	if err != nil {
		klog.Errorf("error getting host for node %s: %v", node.Name, err)
		return makeDeprecatedEnvironmentError(CheckStatusVcenterAPIError, err)
	}
	hostAPIVersion := hostSystem.Config.Product.ApiVersion
	hasRequiredMinimum, err := isMinimumVersion(minRequiredHostVersion, hostAPIVersion)
	if err != nil {
		klog.Errorf("error parsing host version for node %s and host %s: %v", node.Name, hostName, err)
	}
	if !hasRequiredMinimum {
		reason := fmt.Errorf("host %s is on ESXI version %s, which is below minimum required version %s", hostName, hostAPIVersion, minRequiredHostVersion)
		return makeDeprecatedEnvironmentError(CheckStatusDeprecatedESXIVersion, reason)
	}

	hasUpgradeableMinimum, err := isMinimumVersion(minUpgradeableHostVersion, hostAPIVersion)
	if err != nil {
		klog.Errorf("error parsing host version for node %s and host %s: %v", node.Name, hostName, err)
	}
	if !hasUpgradeableMinimum {
		reason := fmt.Errorf("host %s is on ESXI version %s, which is below minimum required version %s for cluster upgrade", hostName, hostAPIVersion, minUpgradeableHostVersion)
		return MakeClusterUnupgradeableError(CheckStatusDeprecatedESXIVersion, reason)
	}

	return MakeClusterCheckResultPass()
}

func (n *NodeChecker) Check(ctx context.Context, checkOpts CheckArgs) []ClusterCheckResult {
	nodes, err := checkOpts.apiClient.ListNodes()
	if err != nil {
		reason := fmt.Errorf("error listing node objects: %v", err)
		return []ClusterCheckResult{MakeClusterDegradedError(CheckStatusOpenshiftAPIError, reason)}
	}

	// Map of host names and their ESXI versions
	n.hostESXIVersions = map[string]bool{}
	n.results = []ClusterCheckResult{}
	workDone := false
	n.createPool(workerCount)

	for i := range nodes {
		node := nodes[i]
		workData := nodeChannelWorkData{
			checkOpts: checkOpts,
			node:      node,
			ctx:       ctx,
		}
		ok := n.addWork(workData)
		if !ok {
			workDone = true
			close(n.workChannel)
			break
		}
	}

	if !workDone {
		close(n.workChannel)
	}
	return n.waitForWorkFinish()
}

func (n *NodeChecker) checkOrMarkHostForProcessing(hostName string) bool {
	n.esxiVersionLock.Lock()
	defer n.esxiVersionLock.Unlock()

	_, found := n.hostESXIVersions[hostName]
	if found {
		return true
	}
	n.hostESXIVersions[hostName] = true
	return false
}

func (n *NodeChecker) getHost(ctx context.Context, checkOpts CheckArgs, hostRef *types.ManagedObjectReference) (mo.HostSystem, error) {
	var o mo.HostSystem
	hostName := hostRef.Value
	hostSystemObject := object.NewHostSystem(checkOpts.vmConnection.Client.Client, *hostRef)

	err := hostSystemObject.Properties(ctx, hostSystemObject.Reference(), []string{"name", "config.product"}, &o)
	if err != nil {
		return o, fmt.Errorf("failed to load ESXi host %s: %v", hostName, err)
	}
	if o.Config == nil {
		return o, fmt.Errorf("error getting ESXi host version %s: host.config is nil", hostName)
	}
	return o, nil
}

func getVM(ctx context.Context, checkOpts CheckArgs, node *v1.Node) (*mo.VirtualMachine, error) {
	vmClient := checkOpts.vmConnection.Client.Client
	vmConfig := checkOpts.vmConnection.Config

	// Find datastore
	finder := find.NewFinder(vmClient, false)
	dc, err := finder.Datacenter(ctx, vmConfig.Workspace.Datacenter)
	if err != nil {
		return nil, fmt.Errorf("failed to access Datacenter %s: %s", vmConfig.Workspace.Datacenter, err)
	}

	// Find VM reference in the datastore, by UUID
	s := object.NewSearchIndex(dc.Client())
	vmUUID := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(node.Spec.ProviderID, "vsphere://")))

	svm, err := s.FindByUuid(ctx, dc, vmUUID, true, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to find VM by UUID %s: %s", vmUUID, err)
	}
	if svm == nil {
		return nil, fmt.Errorf("unable to find VM by UUID %s", vmUUID)
	}

	// Find VM properties
	vm := object.NewVirtualMachine(vmClient, svm.Reference())

	var o mo.VirtualMachine
	err = vm.Properties(ctx, vm.Reference(), nodeProperties, &o)
	if err != nil {
		return nil, fmt.Errorf("failed to load VM %s: %s", node.Name, err)
	}

	return &o, nil
}
