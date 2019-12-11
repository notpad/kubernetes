/*
Copyright 2019 The Kubernetes Authors.

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

package benchmark

import (
	"fmt"
	"io/ioutil"
	v1 "k8s.io/api/core/v1"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/tools/cache"
	"k8s.io/component-base/featuregate"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/klog"
	"k8s.io/kubernetes/test/integration/framework"
	testutils "k8s.io/kubernetes/test/utils"
	"sigs.k8s.io/yaml"
	"sync/atomic"
	"testing"
	"time"
)

var (
	configFile = "config/performance-config.yaml"
)

// TestCase configures a test case to run the scheduler performance test. Users should be able to
// provide this via a YAML file.
//
// It specifies nodes and pods in the cluster before running the test. It also specifies the pods to
// schedule during the test. The config can be as simple as just specify number of nodes/pods, where
// default spec will be applied. It also allows the user to specify a pod spec template for more compicated
// test cases.
//
// It also specifies the metrics to be collected after the test. If nothing is specified, default metrics
// such as scheduling throughput and latencies will be collected.
type TestCase struct {
	// description of the test case
	Desc string
	// configures nodes in the cluster
	Nodes nodeCase
	// configures pods in the cluster before running the tests
	InitPods podCase
	// pods to be scheduled during the test.
	PodsToSchedule podCase
	// optional, feature gates to set before running the test
	FeatureGates map[featuregate.Feature]bool
}

type nodeCase struct {
	Num                      int
	NodeTemplatePath         *string
	NodeAllocatableStrategy  *testutils.NodeAllocatableStrategy
	LabelNodePrepareStrategy *testutils.LabelNodePrepareStrategy
}

type podCase struct {
	Num                               int
	PodTemplatePath                   *string
	ReplicationControllerName         *string
	PersistentVolumeTemplatePath      *string
	PersistentVolumeClaimTemplatePath *string
}

func TestSchedulerPerformance(t *testing.T) {
	var tests []TestCase
	getSpec(&configFile, &tests)

	for _, test := range tests {
		name := fmt.Sprintf("%v/%vNodes/%vPods", test.Desc, test.Nodes.Num, test.InitPods.Num)
		t.Run(name, func(t *testing.T) {
			for feature, flag := range test.FeatureGates {
				defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, feature, flag)()
			}

			var nodeStrategy testutils.PrepareNodeStrategy
			nodeStrategy = &testutils.TrivialNodePrepareStrategy{}

			if test.Nodes.NodeAllocatableStrategy != nil {
				nodeStrategy = test.Nodes.NodeAllocatableStrategy
			} else if test.Nodes.LabelNodePrepareStrategy != nil {
				nodeStrategy = test.Nodes.LabelNodePrepareStrategy
			}

			setupPodStrategy := getPodStrategy(test.InitPods)
			testPodStrategy := getPodStrategy(test.PodsToSchedule)

			var nodeSpec *v1.Node
			if test.Nodes.NodeTemplatePath != nil {
				nodeSpec = &v1.Node{}
				getSpec(test.Nodes.NodeTemplatePath, nodeSpec)
			}

			perfScheduling(test.Nodes.Num, test.InitPods.Num, test.PodsToSchedule.Num, nodeStrategy, setupPodStrategy, testPodStrategy, nodeSpec)
		})
	}
}

func perfScheduling(numNodes, numExistingPods, minPods int,
	nodeStrategy testutils.PrepareNodeStrategy,
	setupPodStrategy, testPodStrategy testutils.TestPodCreateStrategy,
	nodeSpec *v1.Node) {

	finalFunc, podInformer, clientset := mustSetupScheduler()
	defer finalFunc()

	var nodePreparer testutils.TestNodePreparer
	if nodeSpec != nil {
		nodePreparer = framework.NewIntegrationTestNodePreparerWithNodeSpec(
			clientset,
			[]testutils.CountToStrategy{{Count: numNodes, Strategy: nodeStrategy}},
			nodeSpec,
		)
	} else {
		nodePreparer = framework.NewIntegrationTestNodePreparer(
			clientset,
			[]testutils.CountToStrategy{{Count: numNodes, Strategy: nodeStrategy}},
			"scheduler-perf-",
		)
	}

	if err := nodePreparer.PrepareNodes(); err != nil {
		klog.Fatalf("%v", err)
	}
	defer nodePreparer.CleanupNodes()

	config := testutils.NewTestPodCreatorConfig()
	config.AddStrategy(setupNamespace, numExistingPods, setupPodStrategy)
	podCreator := testutils.NewTestPodCreator(clientset, config)
	podCreator.CreatePods()

	for {
		scheduled, err := getScheduledPods(podInformer)
		if err != nil {
			klog.Fatalf("%v", err)
		}
		if len(scheduled) >= numExistingPods {
			break
		}
		klog.Infof("got %d existing pods, required: %d", len(scheduled), numExistingPods)
		time.Sleep(1 * time.Second)
	}

	scheduled := int32(0)
	completedCh := make(chan struct{})
	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(old, cur interface{}) {
			curPod := cur.(*v1.Pod)
			oldPod := old.(*v1.Pod)

			if len(oldPod.Spec.NodeName) == 0 && len(curPod.Spec.NodeName) > 0 {
				if atomic.AddInt32(&scheduled, 1) >= int32(minPods) {
					completedCh <- struct{}{}
				}
			}
		},
	})

	config = testutils.NewTestPodCreatorConfig()
	config.AddStrategy(testNamespace, minPods, testPodStrategy)
	podCreator = testutils.NewTestPodCreator(clientset, config)
	podCreator.CreatePods()

	<-completedCh
}

func getPodStrategy(pc podCase) testutils.TestPodCreateStrategy {
	basePod := makeBasePod()
	if pc.PodTemplatePath != nil {
		basePod = &v1.Pod{}
		getSpec(pc.PodTemplatePath, basePod)
	}
	if pc.ReplicationControllerName != nil {
		return testutils.NewWithControllerCreatePodStrategy(*pc.ReplicationControllerName, basePod)
	}
	if pc.PersistentVolumeClaimTemplatePath == nil {
		return testutils.NewCustomCreatePodStrategy(basePod)
	}

	pvTemplate := &v1.PersistentVolume{}
	getSpec(pc.PersistentVolumeTemplatePath, pvTemplate)
	pvcTemplate := &v1.PersistentVolumeClaim{}
	getSpec(pc.PersistentVolumeClaimTemplatePath, pvcTemplate)
	return testutils.NewCreatePodWithPersistentVolumeStrategy(pvcTemplate, getCustomVolumeFactory(pvTemplate), basePod)
}

func getSpec(path *string, spec interface{}) {
	bytes, err := ioutil.ReadFile(*path)
	if err != nil {
		klog.Fatalf("%v", err)
	}
	if err := yaml.Unmarshal(bytes, spec); err != nil {
		klog.Fatalf("%v", err)
	}
}

func getCustomVolumeFactory(pvTemplate *v1.PersistentVolume) func(id int) *v1.PersistentVolume {
	return func(id int) *v1.PersistentVolume {
		pv := pvTemplate.DeepCopy()
		volumeID := fmt.Sprintf("vol-%d", id)
		pv.ObjectMeta.Name = volumeID
		pvs := pv.Spec.PersistentVolumeSource
		if pvs.CSI != nil {
			pvs.CSI.VolumeHandle = volumeID
		} else if pvs.AWSElasticBlockStore != nil {
			pvs.AWSElasticBlockStore.VolumeID = volumeID
		}
		return pv
	}
}
