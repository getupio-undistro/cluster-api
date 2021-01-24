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

package controllers

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	bootstrapapi "k8s.io/cluster-bootstrap/token/api"
	"k8s.io/utils/pointer"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1alpha3"
	bootstrapv1 "sigs.k8s.io/cluster-api/bootstrap/kubeadm/api/v1alpha3"
	kubeadmv1beta1 "sigs.k8s.io/cluster-api/bootstrap/kubeadm/types/v1beta1"
	fakeremote "sigs.k8s.io/cluster-api/controllers/remote/fake"
	expv1 "sigs.k8s.io/cluster-api/exp/api/v1alpha3"
	"sigs.k8s.io/cluster-api/feature"
	"sigs.k8s.io/cluster-api/test/helpers"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/secret"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func setupScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	if err := clusterv1.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err := expv1.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err := bootstrapv1.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		panic(err)
	}
	return scheme
}

// MachineToBootstrapMapFunc return kubeadm bootstrap configref name when configref exists
func TestKubeadmConfigReconciler_MachineToBootstrapMapFuncReturn(t *testing.T) {
	g := NewWithT(t)

	cluster := newCluster("my-cluster")
	objs := []client.Object{cluster}
	machineObjs := []client.Object{}
	var expectedConfigName string
	for i := 0; i < 3; i++ {
		m := newMachine(cluster, fmt.Sprintf("my-machine-%d", i))
		configName := fmt.Sprintf("my-config-%d", i)
		if i == 1 {
			c := newKubeadmConfig(m, configName)
			objs = append(objs, m, c)
			expectedConfigName = configName
		} else {
			objs = append(objs, m)
		}
		machineObjs = append(machineObjs, m)
	}
	fakeClient := helpers.NewFakeClientWithScheme(setupScheme(), objs...)
	reconciler := &KubeadmConfigReconciler{
		Client: fakeClient,
	}
	for i := 0; i < 3; i++ {
		o := machineObjs[i]
		configs := reconciler.MachineToBootstrapMapFunc(o)
		if i == 1 {
			g.Expect(configs[0].Name).To(Equal(expectedConfigName))
		} else {
			g.Expect(configs[0].Name).To(Equal(""))
		}
	}
}

// Reconcile returns early if the kubeadm config is ready because it should never re-generate bootstrap data.
func TestKubeadmConfigReconciler_Reconcile_ReturnEarlyIfKubeadmConfigIsReady(t *testing.T) {
	g := NewWithT(t)

	config := newKubeadmConfig(nil, "cfg")
	config.Status.Ready = true

	objects := []client.Object{
		config,
	}
	myclient := helpers.NewFakeClientWithScheme(setupScheme(), objects...)

	k := &KubeadmConfigReconciler{
		Client: myclient,
	}

	request := ctrl.Request{
		NamespacedName: client.ObjectKey{
			Name:      "default",
			Namespace: "cfg",
		},
	}
	result, err := k.Reconcile(ctx, request)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.Requeue).To(BeFalse())
	g.Expect(result.RequeueAfter).To(Equal(time.Duration(0)))
}

// Reconcile returns nil if the referenced Machine cannot be found.
func TestKubeadmConfigReconciler_Reconcile_ReturnNilIfReferencedMachineIsNotFound(t *testing.T) {
	g := NewWithT(t)

	machine := newMachine(nil, "machine")
	config := newKubeadmConfig(machine, "cfg")

	objects := []client.Object{
		// intentionally omitting machine
		config,
	}
	myclient := helpers.NewFakeClientWithScheme(setupScheme(), objects...)

	k := &KubeadmConfigReconciler{
		Client: myclient,
	}

	request := ctrl.Request{
		NamespacedName: client.ObjectKey{
			Namespace: "default",
			Name:      "cfg",
		},
	}
	_, err := k.Reconcile(ctx, request)
	g.Expect(err).To(BeNil())
}

// If the machine has bootstrap data secret reference, there is no need to generate more bootstrap data.
func TestKubeadmConfigReconciler_Reconcile_ReturnEarlyIfMachineHasDataSecretName(t *testing.T) {
	g := NewWithT(t)

	machine := newMachine(nil, "machine")
	machine.Spec.Bootstrap.DataSecretName = pointer.StringPtr("something")

	config := newKubeadmConfig(machine, "cfg")
	objects := []client.Object{
		machine,
		config,
	}
	myclient := helpers.NewFakeClientWithScheme(setupScheme(), objects...)

	k := &KubeadmConfigReconciler{
		Client: myclient,
	}

	request := ctrl.Request{
		NamespacedName: client.ObjectKey{
			Namespace: "default",
			Name:      "cfg",
		},
	}
	result, err := k.Reconcile(ctx, request)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.Requeue).To(BeFalse())
	g.Expect(result.RequeueAfter).To(Equal(time.Duration(0)))
}

// Test the logic to migrate plaintext bootstrap data to a field.
func TestKubeadmConfigReconciler_Reconcile_MigrateToSecret(t *testing.T) {
	g := NewWithT(t)

	cluster := newCluster("cluster")
	cluster.Status.InfrastructureReady = true
	machine := newMachine(cluster, "machine")
	config := newKubeadmConfig(machine, "cfg")
	config.Status.Ready = true
	config.Status.BootstrapData = []byte("test")
	objects := []client.Object{
		cluster,
		machine,
		config,
	}
	myclient := helpers.NewFakeClientWithScheme(setupScheme(), objects...)

	k := &KubeadmConfigReconciler{
		Client: myclient,
	}

	request := ctrl.Request{
		NamespacedName: client.ObjectKey{
			Namespace: "default",
			Name:      "cfg",
		},
	}

	result, err := k.Reconcile(ctx, request)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.Requeue).To(BeFalse())
	g.Expect(result.RequeueAfter).To(Equal(time.Duration(0)))

	g.Expect(k.Client.Get(ctx, client.ObjectKey{Name: config.Name, Namespace: config.Namespace}, config)).To(Succeed())
	g.Expect(config.Status.DataSecretName).NotTo(BeNil())

	secret := &corev1.Secret{}
	g.Expect(k.Client.Get(ctx, client.ObjectKey{Namespace: config.Namespace, Name: *config.Status.DataSecretName}, secret)).To(Succeed())
	g.Expect(secret.Data["value"]).NotTo(Equal("test"))
	g.Expect(secret.Type).To(Equal(clusterv1.ClusterSecretType))
	clusterName := secret.Labels[clusterv1.ClusterLabelName]
	g.Expect(clusterName).To(Equal("cluster"))
}

func TestKubeadmConfigReconciler_ReturnEarlyIfClusterInfraNotReady(t *testing.T) {
	g := NewWithT(t)

	cluster := newCluster("cluster")
	machine := newMachine(cluster, "machine")
	config := newKubeadmConfig(machine, "cfg")

	//cluster infra not ready
	cluster.Status = clusterv1.ClusterStatus{
		InfrastructureReady: false,
	}

	objects := []client.Object{
		cluster,
		machine,
		config,
	}
	myclient := helpers.NewFakeClientWithScheme(setupScheme(), objects...)

	k := &KubeadmConfigReconciler{
		Client: myclient,
	}

	request := ctrl.Request{
		NamespacedName: client.ObjectKey{
			Namespace: "default",
			Name:      "cfg",
		},
	}

	expectedResult := reconcile.Result{}
	actualResult, actualError := k.Reconcile(ctx, request)
	g.Expect(actualResult).To(Equal(expectedResult))
	g.Expect(actualError).NotTo(HaveOccurred())
	assertHasFalseCondition(g, myclient, request, bootstrapv1.DataSecretAvailableCondition, clusterv1.ConditionSeverityInfo, bootstrapv1.WaitingForClusterInfrastructureReason)
}

// Return early If the owning machine does not have an associated cluster
func TestKubeadmConfigReconciler_Reconcile_ReturnEarlyIfMachineHasNoCluster(t *testing.T) {
	g := NewWithT(t)

	machine := newMachine(nil, "machine") // Machine without a cluster
	config := newKubeadmConfig(machine, "cfg")

	objects := []client.Object{
		machine,
		config,
	}
	myclient := helpers.NewFakeClientWithScheme(setupScheme(), objects...)

	k := &KubeadmConfigReconciler{
		Client: myclient,
	}

	request := ctrl.Request{
		NamespacedName: client.ObjectKey{
			Namespace: "default",
			Name:      "cfg",
		},
	}
	_, err := k.Reconcile(ctx, request)
	g.Expect(err).NotTo(HaveOccurred())
}

// This does not expect an error, hoping the machine gets updated with a cluster
func TestKubeadmConfigReconciler_Reconcile_ReturnNilIfMachineDoesNotHaveAssociatedCluster(t *testing.T) {
	g := NewWithT(t)

	machine := newMachine(nil, "machine") // intentionally omitting cluster
	config := newKubeadmConfig(machine, "cfg")

	objects := []client.Object{
		machine,
		config,
	}
	myclient := helpers.NewFakeClientWithScheme(setupScheme(), objects...)

	k := &KubeadmConfigReconciler{
		Client: myclient,
	}

	request := ctrl.Request{
		NamespacedName: client.ObjectKey{
			Namespace: "default",
			Name:      "cfg",
		},
	}
	_, err := k.Reconcile(ctx, request)
	g.Expect(err).NotTo(HaveOccurred())
}

// This does not expect an error, hoping that the associated cluster will be created
func TestKubeadmConfigReconciler_Reconcile_ReturnNilIfAssociatedClusterIsNotFound(t *testing.T) {
	g := NewWithT(t)

	cluster := newCluster("cluster")
	machine := newMachine(cluster, "machine")
	config := newKubeadmConfig(machine, "cfg")

	objects := []client.Object{
		// intentionally omitting cluster
		machine,
		config,
	}
	myclient := helpers.NewFakeClientWithScheme(setupScheme(), objects...)

	k := &KubeadmConfigReconciler{
		Client: myclient,
	}

	request := ctrl.Request{
		NamespacedName: client.ObjectKey{
			Namespace: "default",
			Name:      "cfg",
		},
	}
	_, err := k.Reconcile(ctx, request)
	g.Expect(err).NotTo(HaveOccurred())
}

// If the control plane isn't initialized then there is no cluster for either a worker or control plane node to join.
func TestKubeadmConfigReconciler_Reconcile_RequeueJoiningNodesIfControlPlaneNotInitialized(t *testing.T) {
	cluster := newCluster("cluster")
	cluster.Status.InfrastructureReady = true

	workerMachine := newWorkerMachine(cluster)
	workerJoinConfig := newWorkerJoinKubeadmConfig(workerMachine)

	controlPlaneJoinMachine := newControlPlaneMachine(cluster, "control-plane-join-machine")
	controlPlaneJoinConfig := newControlPlaneJoinKubeadmConfig(controlPlaneJoinMachine, "control-plane-join-cfg")

	testcases := []struct {
		name    string
		request ctrl.Request
		objects []client.Object
	}{
		{
			name: "requeue worker when control plane is not yet initialiezd",
			request: ctrl.Request{
				NamespacedName: client.ObjectKey{
					Namespace: workerJoinConfig.Namespace,
					Name:      workerJoinConfig.Name,
				},
			},
			objects: []client.Object{
				cluster,
				workerMachine,
				workerJoinConfig,
			},
		},
		{
			name: "requeue a secondary control plane when the control plane is not yet initialized",
			request: ctrl.Request{
				NamespacedName: client.ObjectKey{
					Namespace: controlPlaneJoinConfig.Namespace,
					Name:      controlPlaneJoinConfig.Name,
				},
			},
			objects: []client.Object{
				cluster,
				controlPlaneJoinMachine,
				controlPlaneJoinConfig,
			},
		},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)

			myclient := helpers.NewFakeClientWithScheme(setupScheme(), tc.objects...)

			k := &KubeadmConfigReconciler{
				Client:          myclient,
				KubeadmInitLock: &myInitLocker{},
			}

			result, err := k.Reconcile(ctx, tc.request)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(result.Requeue).To(BeFalse())
			g.Expect(result.RequeueAfter).To(Equal(30 * time.Second))
			assertHasFalseCondition(g, myclient, tc.request, bootstrapv1.DataSecretAvailableCondition, clusterv1.ConditionSeverityInfo, bootstrapv1.WaitingForControlPlaneAvailableReason)
		})
	}
}

// This generates cloud-config data but does not test the validity of it.
func TestKubeadmConfigReconciler_Reconcile_GenerateCloudConfigData(t *testing.T) {
	g := NewWithT(t)

	cluster := newCluster("cluster")
	cluster.Status.InfrastructureReady = true

	controlPlaneInitMachine := newControlPlaneMachine(cluster, "control-plane-init-machine")
	controlPlaneInitConfig := newControlPlaneInitKubeadmConfig(controlPlaneInitMachine, "control-plane-init-cfg")

	objects := []client.Object{
		cluster,
		controlPlaneInitMachine,
		controlPlaneInitConfig,
	}
	objects = append(objects, createSecrets(t, cluster, controlPlaneInitConfig)...)

	myclient := helpers.NewFakeClientWithScheme(setupScheme(), objects...)

	k := &KubeadmConfigReconciler{
		Client:          myclient,
		KubeadmInitLock: &myInitLocker{},
	}

	request := ctrl.Request{
		NamespacedName: client.ObjectKey{
			Namespace: "default",
			Name:      "control-plane-init-cfg",
		},
	}
	result, err := k.Reconcile(ctx, request)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.Requeue).To(BeFalse())
	g.Expect(result.RequeueAfter).To(Equal(time.Duration(0)))

	cfg, err := getKubeadmConfig(myclient, "control-plane-init-cfg")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cfg.Status.Ready).To(BeTrue())
	g.Expect(cfg.Status.DataSecretName).NotTo(BeNil())
	g.Expect(cfg.Status.ObservedGeneration).NotTo(BeNil())
	assertHasTrueCondition(g, myclient, request, bootstrapv1.CertificatesAvailableCondition)
	assertHasTrueCondition(g, myclient, request, bootstrapv1.DataSecretAvailableCondition)

	// Ensure that we don't fail trying to refresh any bootstrap tokens
	_, err = k.Reconcile(ctx, request)
	g.Expect(err).NotTo(HaveOccurred())
}

// If a control plane has no JoinConfiguration, then we will create a default and no error will occur
func TestKubeadmConfigReconciler_Reconcile_ErrorIfJoiningControlPlaneHasInvalidConfiguration(t *testing.T) {
	g := NewWithT(t)
	// TODO: extract this kind of code into a setup function that puts the state of objects into an initialized controlplane (implies secrets exist)
	cluster := newCluster("cluster")
	cluster.Status.InfrastructureReady = true
	cluster.Status.ControlPlaneInitialized = true
	cluster.Spec.ControlPlaneEndpoint = clusterv1.APIEndpoint{Host: "100.105.150.1", Port: 6443}
	controlPlaneInitMachine := newControlPlaneMachine(cluster, "control-plane-init-machine")
	controlPlaneInitConfig := newControlPlaneInitKubeadmConfig(controlPlaneInitMachine, "control-plane-init-cfg")

	controlPlaneJoinMachine := newControlPlaneMachine(cluster, "control-plane-join-machine")
	controlPlaneJoinConfig := newControlPlaneJoinKubeadmConfig(controlPlaneJoinMachine, "control-plane-join-cfg")
	controlPlaneJoinConfig.Spec.JoinConfiguration.ControlPlane = nil // Makes controlPlaneJoinConfig invalid for a control plane machine

	objects := []client.Object{
		cluster,
		controlPlaneJoinMachine,
		controlPlaneJoinConfig,
	}
	objects = append(objects, createSecrets(t, cluster, controlPlaneInitConfig)...)
	myclient := helpers.NewFakeClientWithScheme(setupScheme(), objects...)

	k := &KubeadmConfigReconciler{
		Client:             myclient,
		KubeadmInitLock:    &myInitLocker{},
		remoteClientGetter: fakeremote.NewClusterClient,
	}

	request := ctrl.Request{
		NamespacedName: client.ObjectKey{
			Namespace: "default",
			Name:      "control-plane-join-cfg",
		},
	}
	_, err := k.Reconcile(ctx, request)
	g.Expect(err).NotTo(HaveOccurred())
}

// If there is no APIEndpoint but everything is ready then requeue in hopes of a new APIEndpoint showing up eventually.
func TestKubeadmConfigReconciler_Reconcile_RequeueIfControlPlaneIsMissingAPIEndpoints(t *testing.T) {
	g := NewWithT(t)

	cluster := newCluster("cluster")
	cluster.Status.InfrastructureReady = true
	cluster.Status.ControlPlaneInitialized = true
	controlPlaneInitMachine := newControlPlaneMachine(cluster, "control-plane-init-machine")
	controlPlaneInitConfig := newControlPlaneInitKubeadmConfig(controlPlaneInitMachine, "control-plane-init-cfg")

	workerMachine := newWorkerMachine(cluster)
	workerJoinConfig := newWorkerJoinKubeadmConfig(workerMachine)

	objects := []client.Object{
		cluster,
		workerMachine,
		workerJoinConfig,
	}
	objects = append(objects, createSecrets(t, cluster, controlPlaneInitConfig)...)

	myclient := helpers.NewFakeClientWithScheme(setupScheme(), objects...)

	k := &KubeadmConfigReconciler{
		Client:          myclient,
		KubeadmInitLock: &myInitLocker{},
	}

	request := ctrl.Request{
		NamespacedName: client.ObjectKey{
			Namespace: "default",
			Name:      "worker-join-cfg",
		},
	}
	result, err := k.Reconcile(ctx, request)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.Requeue).To(BeFalse())
	g.Expect(result.RequeueAfter).To(Equal(10 * time.Second))
}

func TestReconcileIfJoinNodesAndControlPlaneIsReady(t *testing.T) {
	cluster := newCluster("cluster")
	cluster.Status.InfrastructureReady = true
	cluster.Status.ControlPlaneInitialized = true
	cluster.Spec.ControlPlaneEndpoint = clusterv1.APIEndpoint{Host: "100.105.150.1", Port: 6443}

	var useCases = []struct {
		name          string
		machine       *clusterv1.Machine
		configName    string
		configBuilder func(*clusterv1.Machine, string) *bootstrapv1.KubeadmConfig
	}{
		{
			name:       "Join a worker node with a fully compiled kubeadm config object",
			machine:    newWorkerMachine(cluster),
			configName: "worker-join-cfg",
			configBuilder: func(machine *clusterv1.Machine, name string) *bootstrapv1.KubeadmConfig {
				return newWorkerJoinKubeadmConfig(machine)
			},
		},
		{
			name:          "Join a worker node  with an empty kubeadm config object (defaults apply)",
			machine:       newWorkerMachine(cluster),
			configName:    "worker-join-cfg",
			configBuilder: newKubeadmConfig,
		},
		{
			name:          "Join a control plane node with a fully compiled kubeadm config object",
			machine:       newControlPlaneMachine(cluster, "control-plane-join-machine"),
			configName:    "control-plane-join-cfg",
			configBuilder: newControlPlaneJoinKubeadmConfig,
		},
		{
			name:          "Join a control plane node with an empty kubeadm config object (defaults apply)",
			machine:       newControlPlaneMachine(cluster, "control-plane-join-machine"),
			configName:    "control-plane-join-cfg",
			configBuilder: newKubeadmConfig,
		},
	}

	for _, rt := range useCases {
		rt := rt // pin!
		t.Run(rt.name, func(t *testing.T) {
			g := NewWithT(t)

			config := rt.configBuilder(rt.machine, rt.configName)

			objects := []client.Object{
				cluster,
				rt.machine,
				config,
			}
			objects = append(objects, createSecrets(t, cluster, config)...)
			myclient := helpers.NewFakeClientWithScheme(setupScheme(), objects...)
			k := &KubeadmConfigReconciler{
				Client:             myclient,
				KubeadmInitLock:    &myInitLocker{},
				remoteClientGetter: fakeremote.NewClusterClient,
			}

			request := ctrl.Request{
				NamespacedName: client.ObjectKey{
					Namespace: config.GetNamespace(),
					Name:      rt.configName,
				},
			}
			result, err := k.Reconcile(ctx, request)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(result.Requeue).To(BeFalse())
			g.Expect(result.RequeueAfter).To(Equal(time.Duration(0)))

			cfg, err := getKubeadmConfig(myclient, rt.configName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(cfg.Status.Ready).To(BeTrue())
			g.Expect(cfg.Status.DataSecretName).NotTo(BeNil())
			g.Expect(cfg.Status.ObservedGeneration).NotTo(BeNil())
			assertHasTrueCondition(g, myclient, request, bootstrapv1.DataSecretAvailableCondition)

			l := &corev1.SecretList{}
			err = myclient.List(ctx, l, client.ListOption(client.InNamespace(metav1.NamespaceSystem)))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(len(l.Items)).To(Equal(1))
		})

	}
}

func TestReconcileIfJoinNodePoolsAndControlPlaneIsReady(t *testing.T) {
	_ = feature.MutableGates.Set("MachinePool=true")

	cluster := newCluster("cluster")
	cluster.Status.InfrastructureReady = true
	cluster.Status.ControlPlaneInitialized = true
	cluster.Spec.ControlPlaneEndpoint = clusterv1.APIEndpoint{Host: "100.105.150.1", Port: 6443}

	var useCases = []struct {
		name          string
		machinePool   *expv1.MachinePool
		configName    string
		configBuilder func(*expv1.MachinePool, string) *bootstrapv1.KubeadmConfig
	}{
		{
			name:        "Join a worker node with a fully compiled kubeadm config object",
			machinePool: newWorkerMachinePool(cluster),
			configName:  "workerpool-join-cfg",
			configBuilder: func(machinePool *expv1.MachinePool, name string) *bootstrapv1.KubeadmConfig {
				return newWorkerPoolJoinKubeadmConfig(machinePool)
			},
		},
		{
			name:          "Join a worker node  with an empty kubeadm config object (defaults apply)",
			machinePool:   newWorkerMachinePool(cluster),
			configName:    "workerpool-join-cfg",
			configBuilder: newMachinePoolKubeadmConfig,
		},
	}

	for _, rt := range useCases {
		rt := rt // pin!
		t.Run(rt.name, func(t *testing.T) {
			g := NewWithT(t)

			config := rt.configBuilder(rt.machinePool, rt.configName)

			objects := []client.Object{
				cluster,
				rt.machinePool,
				config,
			}
			objects = append(objects, createSecrets(t, cluster, config)...)
			myclient := helpers.NewFakeClientWithScheme(setupScheme(), objects...)
			k := &KubeadmConfigReconciler{
				Client:             myclient,
				KubeadmInitLock:    &myInitLocker{},
				remoteClientGetter: fakeremote.NewClusterClient,
			}

			request := ctrl.Request{
				NamespacedName: client.ObjectKey{
					Namespace: config.GetNamespace(),
					Name:      rt.configName,
				},
			}
			result, err := k.Reconcile(ctx, request)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(result.Requeue).To(BeFalse())
			g.Expect(result.RequeueAfter).To(Equal(time.Duration(0)))

			cfg, err := getKubeadmConfig(myclient, rt.configName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(cfg.Status.Ready).To(BeTrue())
			g.Expect(cfg.Status.DataSecretName).NotTo(BeNil())
			g.Expect(cfg.Status.ObservedGeneration).NotTo(BeNil())

			l := &corev1.SecretList{}
			err = myclient.List(ctx, l, client.ListOption(client.InNamespace(metav1.NamespaceSystem)))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(len(l.Items)).To(Equal(1))
		})

	}
}

// during kubeadmconfig reconcile it is possible that bootstrap secret gets created
// but kubeadmconfig is not patched, do not error if secret already exists.
// ignore the alreadyexists error and update the status to ready.
func TestKubeadmConfigSecretCreatedStatusNotPatched(t *testing.T) {
	g := NewWithT(t)

	cluster := newCluster("cluster")
	cluster.Status.InfrastructureReady = true
	cluster.Status.ControlPlaneInitialized = true
	cluster.Spec.ControlPlaneEndpoint = clusterv1.APIEndpoint{Host: "100.105.150.1", Port: 6443}

	controlPlaneInitMachine := newControlPlaneMachine(cluster, "control-plane-init-machine")
	initConfig := newControlPlaneInitKubeadmConfig(controlPlaneInitMachine, "control-plane-init-config")
	workerMachine := newWorkerMachine(cluster)
	workerJoinConfig := newWorkerJoinKubeadmConfig(workerMachine)
	objects := []client.Object{
		cluster,
		workerMachine,
		workerJoinConfig,
	}

	objects = append(objects, createSecrets(t, cluster, initConfig)...)
	myclient := helpers.NewFakeClientWithScheme(setupScheme(), objects...)
	k := &KubeadmConfigReconciler{
		Client:             myclient,
		KubeadmInitLock:    &myInitLocker{},
		remoteClientGetter: fakeremote.NewClusterClient,
	}
	request := ctrl.Request{
		NamespacedName: client.ObjectKey{
			Namespace: "default",
			Name:      "worker-join-cfg",
		},
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      workerJoinConfig.Name,
			Namespace: workerJoinConfig.Namespace,
			Labels: map[string]string{
				clusterv1.ClusterLabelName: cluster.Name,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: bootstrapv1.GroupVersion.String(),
					Kind:       "KubeadmConfig",
					Name:       workerJoinConfig.Name,
					UID:        workerJoinConfig.UID,
					Controller: pointer.BoolPtr(true),
				},
			},
		},
		Data: map[string][]byte{
			"value": nil,
		},
		Type: clusterv1.ClusterSecretType,
	}

	err := myclient.Create(ctx, secret)
	g.Expect(err).ToNot(HaveOccurred())
	result, err := k.Reconcile(ctx, request)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.Requeue).To(BeFalse())
	g.Expect(result.RequeueAfter).To(Equal(time.Duration(0)))

	cfg, err := getKubeadmConfig(myclient, "worker-join-cfg")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cfg.Status.Ready).To(BeTrue())
	g.Expect(cfg.Status.DataSecretName).NotTo(BeNil())
	g.Expect(cfg.Status.ObservedGeneration).NotTo(BeNil())
}

func TestBootstrapTokenTTLExtension(t *testing.T) {
	t.Skip("This now fails because it's using Update instead of patch, needs rework")

	g := NewWithT(t)

	cluster := newCluster("cluster")
	cluster.Status.InfrastructureReady = true
	cluster.Status.ControlPlaneInitialized = true
	cluster.Spec.ControlPlaneEndpoint = clusterv1.APIEndpoint{Host: "100.105.150.1", Port: 6443}

	controlPlaneInitMachine := newControlPlaneMachine(cluster, "control-plane-init-machine")
	initConfig := newControlPlaneInitKubeadmConfig(controlPlaneInitMachine, "control-plane-init-config")
	workerMachine := newWorkerMachine(cluster)
	workerJoinConfig := newWorkerJoinKubeadmConfig(workerMachine)
	controlPlaneJoinMachine := newControlPlaneMachine(cluster, "control-plane-join-machine")
	controlPlaneJoinConfig := newControlPlaneJoinKubeadmConfig(controlPlaneJoinMachine, "control-plane-join-cfg")
	objects := []client.Object{
		cluster,
		workerMachine,
		workerJoinConfig,
		controlPlaneJoinMachine,
		controlPlaneJoinConfig,
	}

	objects = append(objects, createSecrets(t, cluster, initConfig)...)
	myclient := helpers.NewFakeClientWithScheme(setupScheme(), objects...)
	k := &KubeadmConfigReconciler{
		Client:             myclient,
		KubeadmInitLock:    &myInitLocker{},
		remoteClientGetter: fakeremote.NewClusterClient,
	}
	request := ctrl.Request{
		NamespacedName: client.ObjectKey{
			Namespace: "default",
			Name:      "worker-join-cfg",
		},
	}
	result, err := k.Reconcile(ctx, request)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.Requeue).To(BeFalse())
	g.Expect(result.RequeueAfter).To(Equal(time.Duration(0)))

	cfg, err := getKubeadmConfig(myclient, "worker-join-cfg")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cfg.Status.Ready).To(BeTrue())
	g.Expect(cfg.Status.DataSecretName).NotTo(BeNil())
	g.Expect(cfg.Status.ObservedGeneration).NotTo(BeNil())

	request = ctrl.Request{
		NamespacedName: client.ObjectKey{
			Namespace: "default",
			Name:      "control-plane-join-cfg",
		},
	}
	result, err = k.Reconcile(ctx, request)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.Requeue).To(BeFalse())
	g.Expect(result.RequeueAfter).To(Equal(time.Duration(0)))

	cfg, err = getKubeadmConfig(myclient, "control-plane-join-cfg")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cfg.Status.Ready).To(BeTrue())
	g.Expect(cfg.Status.DataSecretName).NotTo(BeNil())
	g.Expect(cfg.Status.ObservedGeneration).NotTo(BeNil())

	l := &corev1.SecretList{}
	err = myclient.List(ctx, l, client.ListOption(client.InNamespace(metav1.NamespaceSystem)))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(len(l.Items)).To(Equal(2))

	// ensure that the token is refreshed...
	tokenExpires := make([][]byte, len(l.Items))

	for i, item := range l.Items {
		tokenExpires[i] = item.Data[bootstrapapi.BootstrapTokenExpirationKey]
	}

	<-time.After(1 * time.Second)

	for _, req := range []ctrl.Request{
		{
			NamespacedName: client.ObjectKey{
				Namespace: "default",
				Name:      "worker-join-cfg",
			},
		},
		{
			NamespacedName: client.ObjectKey{
				Namespace: "default",
				Name:      "control-plane-join-cfg",
			},
		},
	} {

		result, err := k.Reconcile(ctx, req)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(result.RequeueAfter).NotTo(BeNumerically(">=", DefaultTokenTTL))
	}

	l = &corev1.SecretList{}
	err = myclient.List(ctx, l, client.ListOption(client.InNamespace(metav1.NamespaceSystem)))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(len(l.Items)).To(Equal(2))

	for i, item := range l.Items {
		g.Expect(bytes.Equal(tokenExpires[i], item.Data[bootstrapapi.BootstrapTokenExpirationKey])).To(BeFalse())
		tokenExpires[i] = item.Data[bootstrapapi.BootstrapTokenExpirationKey]
	}

	// ...until the infrastructure is marked "ready"
	workerMachine.Status.InfrastructureReady = true
	err = myclient.Update(ctx, workerMachine)
	g.Expect(err).NotTo(HaveOccurred())

	controlPlaneJoinMachine.Status.InfrastructureReady = true
	err = myclient.Update(ctx, controlPlaneJoinMachine)
	g.Expect(err).NotTo(HaveOccurred())

	<-time.After(1 * time.Second)

	for _, req := range []ctrl.Request{
		{
			NamespacedName: client.ObjectKey{
				Namespace: "default",
				Name:      "worker-join-cfg",
			},
		},
		{
			NamespacedName: client.ObjectKey{
				Namespace: "default",
				Name:      "control-plane-join-cfg",
			},
		},
	} {

		result, err := k.Reconcile(ctx, req)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(result.Requeue).To(BeFalse())
		g.Expect(result.RequeueAfter).To(Equal(time.Duration(0)))
	}

	l = &corev1.SecretList{}
	err = myclient.List(ctx, l, client.ListOption(client.InNamespace(metav1.NamespaceSystem)))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(len(l.Items)).To(Equal(2))

	for i, item := range l.Items {
		g.Expect(bytes.Equal(tokenExpires[i], item.Data[bootstrapapi.BootstrapTokenExpirationKey])).To(BeTrue())
	}
}

// Ensure the discovery portion of the JoinConfiguration gets generated correctly.
func TestKubeadmConfigReconciler_Reconcile_DiscoveryReconcileBehaviors(t *testing.T) {
	k := &KubeadmConfigReconciler{
		Client:             helpers.NewFakeClientWithScheme(setupScheme()),
		KubeadmInitLock:    &myInitLocker{},
		remoteClientGetter: fakeremote.NewClusterClient,
	}

	caHash := []string{"...."}
	bootstrapToken := kubeadmv1beta1.Discovery{
		BootstrapToken: &kubeadmv1beta1.BootstrapTokenDiscovery{
			CACertHashes: caHash,
		},
	}
	goodcluster := &clusterv1.Cluster{
		Spec: clusterv1.ClusterSpec{
			ControlPlaneEndpoint: clusterv1.APIEndpoint{
				Host: "example.com",
				Port: 6443,
			},
		},
	}
	testcases := []struct {
		name              string
		cluster           *clusterv1.Cluster
		config            *bootstrapv1.KubeadmConfig
		validateDiscovery func(*WithT, *bootstrapv1.KubeadmConfig) error
	}{
		{
			name:    "Automatically generate token if discovery not specified",
			cluster: goodcluster,
			config: &bootstrapv1.KubeadmConfig{
				Spec: bootstrapv1.KubeadmConfigSpec{
					JoinConfiguration: &kubeadmv1beta1.JoinConfiguration{
						Discovery: bootstrapToken,
					},
				},
			},
			validateDiscovery: func(g *WithT, c *bootstrapv1.KubeadmConfig) error {
				d := c.Spec.JoinConfiguration.Discovery
				g.Expect(d.BootstrapToken).NotTo(BeNil())
				g.Expect(d.BootstrapToken.Token).NotTo(Equal(""))
				g.Expect(d.BootstrapToken.APIServerEndpoint).To(Equal("example.com:6443"))
				g.Expect(d.BootstrapToken.UnsafeSkipCAVerification).To(BeFalse())
				return nil
			},
		},
		{
			name:    "Respect discoveryConfiguration.File",
			cluster: goodcluster,
			config: &bootstrapv1.KubeadmConfig{
				Spec: bootstrapv1.KubeadmConfigSpec{
					JoinConfiguration: &kubeadmv1beta1.JoinConfiguration{
						Discovery: kubeadmv1beta1.Discovery{
							File: &kubeadmv1beta1.FileDiscovery{},
						},
					},
				},
			},
			validateDiscovery: func(g *WithT, c *bootstrapv1.KubeadmConfig) error {
				d := c.Spec.JoinConfiguration.Discovery
				g.Expect(d.BootstrapToken).To(BeNil())
				return nil
			},
		},
		{
			name:    "Respect discoveryConfiguration.BootstrapToken.APIServerEndpoint",
			cluster: goodcluster,
			config: &bootstrapv1.KubeadmConfig{
				Spec: bootstrapv1.KubeadmConfigSpec{
					JoinConfiguration: &kubeadmv1beta1.JoinConfiguration{
						Discovery: kubeadmv1beta1.Discovery{
							BootstrapToken: &kubeadmv1beta1.BootstrapTokenDiscovery{
								CACertHashes:      caHash,
								APIServerEndpoint: "bar.com:6443",
							},
						},
					},
				},
			},
			validateDiscovery: func(g *WithT, c *bootstrapv1.KubeadmConfig) error {
				d := c.Spec.JoinConfiguration.Discovery
				g.Expect(d.BootstrapToken.APIServerEndpoint).To(Equal("bar.com:6443"))
				return nil
			},
		},
		{
			name:    "Respect discoveryConfiguration.BootstrapToken.Token",
			cluster: goodcluster,
			config: &bootstrapv1.KubeadmConfig{
				Spec: bootstrapv1.KubeadmConfigSpec{
					JoinConfiguration: &kubeadmv1beta1.JoinConfiguration{
						Discovery: kubeadmv1beta1.Discovery{
							BootstrapToken: &kubeadmv1beta1.BootstrapTokenDiscovery{
								CACertHashes: caHash,
								Token:        "abcdef.0123456789abcdef",
							},
						},
					},
				},
			},
			validateDiscovery: func(g *WithT, c *bootstrapv1.KubeadmConfig) error {
				d := c.Spec.JoinConfiguration.Discovery
				g.Expect(d.BootstrapToken.Token).To(Equal("abcdef.0123456789abcdef"))
				return nil
			},
		},
		{
			name:    "Respect discoveryConfiguration.BootstrapToken.CACertHashes",
			cluster: goodcluster,
			config: &bootstrapv1.KubeadmConfig{
				Spec: bootstrapv1.KubeadmConfigSpec{
					JoinConfiguration: &kubeadmv1beta1.JoinConfiguration{
						Discovery: kubeadmv1beta1.Discovery{
							BootstrapToken: &kubeadmv1beta1.BootstrapTokenDiscovery{
								CACertHashes: caHash,
							},
						},
					},
				},
			},
			validateDiscovery: func(g *WithT, c *bootstrapv1.KubeadmConfig) error {
				d := c.Spec.JoinConfiguration.Discovery
				g.Expect(reflect.DeepEqual(d.BootstrapToken.CACertHashes, caHash)).To(BeTrue())
				return nil
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)

			res, err := k.reconcileDiscovery(ctx, tc.cluster, tc.config, secret.Certificates{})
			g.Expect(res.IsZero()).To(BeTrue())
			g.Expect(err).NotTo(HaveOccurred())

			err = tc.validateDiscovery(g, tc.config)
			g.Expect(err).NotTo(HaveOccurred())
		})
	}
}

// Test failure cases for the discovery reconcile function.
func TestKubeadmConfigReconciler_Reconcile_DiscoveryReconcileFailureBehaviors(t *testing.T) {
	k := &KubeadmConfigReconciler{}

	testcases := []struct {
		name    string
		cluster *clusterv1.Cluster
		config  *bootstrapv1.KubeadmConfig

		result ctrl.Result
		err    error
	}{
		{
			name:    "Should requeue if cluster has not ControlPlaneEndpoint",
			cluster: &clusterv1.Cluster{}, // cluster without endpoints
			config: &bootstrapv1.KubeadmConfig{
				Spec: bootstrapv1.KubeadmConfigSpec{
					JoinConfiguration: &kubeadmv1beta1.JoinConfiguration{
						Discovery: kubeadmv1beta1.Discovery{
							BootstrapToken: &kubeadmv1beta1.BootstrapTokenDiscovery{
								CACertHashes: []string{"item"},
							},
						},
					},
				},
			},
			result: ctrl.Result{RequeueAfter: 10 * time.Second},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)

			res, err := k.reconcileDiscovery(ctx, tc.cluster, tc.config, secret.Certificates{})
			g.Expect(res).To(Equal(tc.result))
			if tc.err == nil {
				g.Expect(err).To(BeNil())
			} else {
				g.Expect(err).To(Equal(tc.err))
			}
		})
	}
}

// Set cluster configuration defaults based on dynamic values from the cluster object.
func TestKubeadmConfigReconciler_Reconcile_DynamicDefaultsForClusterConfiguration(t *testing.T) {
	k := &KubeadmConfigReconciler{}

	testcases := []struct {
		name    string
		cluster *clusterv1.Cluster
		machine *clusterv1.Machine
		config  *bootstrapv1.KubeadmConfig
	}{
		{
			name: "Config settings have precedence",
			config: &bootstrapv1.KubeadmConfig{
				Spec: bootstrapv1.KubeadmConfigSpec{
					ClusterConfiguration: &kubeadmv1beta1.ClusterConfiguration{
						ClusterName:       "mycluster",
						KubernetesVersion: "myversion",
						Networking: kubeadmv1beta1.Networking{
							PodSubnet:     "myPodSubnet",
							ServiceSubnet: "myServiceSubnet",
							DNSDomain:     "myDNSDomain",
						},
						ControlPlaneEndpoint: "myControlPlaneEndpoint:6443",
					},
				},
			},
			cluster: &clusterv1.Cluster{
				ObjectMeta: metav1.ObjectMeta{
					Name: "OtherName",
				},
				Spec: clusterv1.ClusterSpec{
					ClusterNetwork: &clusterv1.ClusterNetwork{
						Services:      &clusterv1.NetworkRanges{CIDRBlocks: []string{"otherServicesCidr"}},
						Pods:          &clusterv1.NetworkRanges{CIDRBlocks: []string{"otherPodsCidr"}},
						ServiceDomain: "otherServiceDomain",
					},
					ControlPlaneEndpoint: clusterv1.APIEndpoint{Host: "otherVersion", Port: 0},
				},
			},
			machine: &clusterv1.Machine{
				Spec: clusterv1.MachineSpec{
					Version: pointer.StringPtr("otherVersion"),
				},
			},
		},
		{
			name: "Top level object settings are used in case config settings are missing",
			config: &bootstrapv1.KubeadmConfig{
				Spec: bootstrapv1.KubeadmConfigSpec{
					ClusterConfiguration: &kubeadmv1beta1.ClusterConfiguration{},
				},
			},
			cluster: &clusterv1.Cluster{
				ObjectMeta: metav1.ObjectMeta{
					Name: "mycluster",
				},
				Spec: clusterv1.ClusterSpec{
					ClusterNetwork: &clusterv1.ClusterNetwork{
						Services:      &clusterv1.NetworkRanges{CIDRBlocks: []string{"myServiceSubnet"}},
						Pods:          &clusterv1.NetworkRanges{CIDRBlocks: []string{"myPodSubnet"}},
						ServiceDomain: "myDNSDomain",
					},
					ControlPlaneEndpoint: clusterv1.APIEndpoint{Host: "myControlPlaneEndpoint", Port: 6443},
				},
			},
			machine: &clusterv1.Machine{
				Spec: clusterv1.MachineSpec{
					Version: pointer.StringPtr("myversion"),
				},
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)

			k.reconcileTopLevelObjectSettings(ctx, tc.cluster, tc.machine, tc.config)

			g.Expect(tc.config.Spec.ClusterConfiguration.ControlPlaneEndpoint).To(Equal("myControlPlaneEndpoint:6443"))
			g.Expect(tc.config.Spec.ClusterConfiguration.ClusterName).To(Equal("mycluster"))
			g.Expect(tc.config.Spec.ClusterConfiguration.Networking.PodSubnet).To(Equal("myPodSubnet"))
			g.Expect(tc.config.Spec.ClusterConfiguration.Networking.ServiceSubnet).To(Equal("myServiceSubnet"))
			g.Expect(tc.config.Spec.ClusterConfiguration.Networking.DNSDomain).To(Equal("myDNSDomain"))
			g.Expect(tc.config.Spec.ClusterConfiguration.KubernetesVersion).To(Equal("myversion"))
		})
	}
}

// Allow users to skip CA Verification if they *really* want to.
func TestKubeadmConfigReconciler_Reconcile_AlwaysCheckCAVerificationUnlessRequestedToSkip(t *testing.T) {
	// Setup work for an initialized cluster
	clusterName := "my-cluster"
	cluster := newCluster(clusterName)
	cluster.Status.ControlPlaneInitialized = true
	cluster.Status.InfrastructureReady = true
	cluster.Spec.ControlPlaneEndpoint = clusterv1.APIEndpoint{
		Host: "example.com",
		Port: 6443,
	}
	controlPlaneInitMachine := newControlPlaneMachine(cluster, "my-control-plane-init-machine")
	initConfig := newControlPlaneInitKubeadmConfig(controlPlaneInitMachine, "my-control-plane-init-config")

	controlPlaneMachineName := "my-machine"
	machine := newMachine(cluster, controlPlaneMachineName)

	workerMachineName := "my-worker"
	workerMachine := newMachine(cluster, workerMachineName)

	controlPlaneConfigName := "my-config"
	config := newKubeadmConfig(machine, controlPlaneConfigName)

	objects := []client.Object{
		cluster, machine, workerMachine, config,
	}
	objects = append(objects, createSecrets(t, cluster, initConfig)...)

	testcases := []struct {
		name               string
		discovery          *kubeadmv1beta1.BootstrapTokenDiscovery
		skipCAVerification bool
	}{
		{
			name:               "Do not skip CA verification by default",
			discovery:          &kubeadmv1beta1.BootstrapTokenDiscovery{},
			skipCAVerification: false,
		},
		{
			name: "Skip CA verification if requested by the user",
			discovery: &kubeadmv1beta1.BootstrapTokenDiscovery{
				UnsafeSkipCAVerification: true,
			},
			skipCAVerification: true,
		},
		{
			// skipCAVerification should be true since no Cert Hashes are provided, but reconcile will *always* get or create certs.
			// TODO: Certificate get/create behavior needs to be mocked to enable this test.
			name: "cannot test for defaulting behavior through the reconcile function",
			discovery: &kubeadmv1beta1.BootstrapTokenDiscovery{
				CACertHashes: []string{""},
			},
			skipCAVerification: false,
		},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)

			myclient := helpers.NewFakeClientWithScheme(setupScheme(), objects...)
			reconciler := KubeadmConfigReconciler{
				Client:             myclient,
				KubeadmInitLock:    &myInitLocker{},
				remoteClientGetter: fakeremote.NewClusterClient,
			}

			wc := newWorkerJoinKubeadmConfig(workerMachine)
			wc.Spec.JoinConfiguration.Discovery.BootstrapToken = tc.discovery
			key := client.ObjectKey{Namespace: wc.Namespace, Name: wc.Name}
			err := myclient.Create(ctx, wc)
			g.Expect(err).NotTo(HaveOccurred())

			req := ctrl.Request{NamespacedName: key}
			_, err = reconciler.Reconcile(ctx, req)
			g.Expect(err).NotTo(HaveOccurred())

			cfg := &bootstrapv1.KubeadmConfig{}
			err = myclient.Get(ctx, key, cfg)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(cfg.Spec.JoinConfiguration.Discovery.BootstrapToken.UnsafeSkipCAVerification).To(Equal(tc.skipCAVerification))
		})
	}
}

// If a cluster object changes then all associated KubeadmConfigs should be re-reconciled.
// This allows us to not requeue a kubeadm config while we wait for InfrastructureReady.
func TestKubeadmConfigReconciler_ClusterToKubeadmConfigs(t *testing.T) {
	_ = feature.MutableGates.Set("MachinePool=true")
	g := NewWithT(t)

	cluster := newCluster("my-cluster")
	objs := []client.Object{cluster}
	expectedNames := []string{}
	for i := 0; i < 3; i++ {
		m := newMachine(cluster, fmt.Sprintf("my-machine-%d", i))
		configName := fmt.Sprintf("my-config-%d", i)
		c := newKubeadmConfig(m, configName)
		expectedNames = append(expectedNames, configName)
		objs = append(objs, m, c)
	}
	for i := 3; i < 6; i++ {
		mp := newMachinePool(cluster, fmt.Sprintf("my-machinepool-%d", i))
		configName := fmt.Sprintf("my-config-%d", i)
		c := newMachinePoolKubeadmConfig(mp, configName)
		expectedNames = append(expectedNames, configName)
		objs = append(objs, mp, c)
	}
	fakeClient := helpers.NewFakeClientWithScheme(setupScheme(), objs...)
	reconciler := &KubeadmConfigReconciler{
		Client: fakeClient,
	}
	configs := reconciler.ClusterToKubeadmConfigs(cluster)
	names := make([]string, 6)
	for i := range configs {
		names[i] = configs[i].Name
	}
	for _, name := range expectedNames {
		found := false
		for _, foundName := range names {
			if foundName == name {
				found = true
			}
		}
		g.Expect(found).To(BeTrue())
	}
}

// Reconcile should not fail if the Etcd CA Secret already exists
func TestKubeadmConfigReconciler_Reconcile_DoesNotFailIfCASecretsAlreadyExist(t *testing.T) {
	g := NewWithT(t)

	cluster := newCluster("my-cluster")
	cluster.Status.InfrastructureReady = true
	cluster.Status.ControlPlaneInitialized = false
	m := newControlPlaneMachine(cluster, "control-plane-machine")
	configName := "my-config"
	c := newControlPlaneInitKubeadmConfig(m, configName)
	scrt := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", cluster.Name, secret.EtcdCA),
			Namespace: "default",
		},
		Data: map[string][]byte{
			"tls.crt": []byte("hello world"),
			"tls.key": []byte("hello world"),
		},
	}
	fakec := helpers.NewFakeClientWithScheme(setupScheme(), []client.Object{cluster, m, c, scrt}...)
	reconciler := &KubeadmConfigReconciler{
		Client:          fakec,
		KubeadmInitLock: &myInitLocker{},
	}
	req := ctrl.Request{
		NamespacedName: client.ObjectKey{Namespace: "default", Name: configName},
	}
	_, err := reconciler.Reconcile(ctx, req)
	g.Expect(err).NotTo(HaveOccurred())
}

// Exactly one control plane machine initializes if there are multiple control plane machines defined
func TestKubeadmConfigReconciler_Reconcile_ExactlyOneControlPlaneMachineInitializes(t *testing.T) {
	g := NewWithT(t)

	cluster := newCluster("cluster")
	cluster.Status.InfrastructureReady = true

	controlPlaneInitMachineFirst := newControlPlaneMachine(cluster, "control-plane-init-machine-first")
	controlPlaneInitConfigFirst := newControlPlaneInitKubeadmConfig(controlPlaneInitMachineFirst, "control-plane-init-cfg-first")

	controlPlaneInitMachineSecond := newControlPlaneMachine(cluster, "control-plane-init-machine-second")
	controlPlaneInitConfigSecond := newControlPlaneInitKubeadmConfig(controlPlaneInitMachineSecond, "control-plane-init-cfg-second")

	objects := []client.Object{
		cluster,
		controlPlaneInitMachineFirst,
		controlPlaneInitConfigFirst,
		controlPlaneInitMachineSecond,
		controlPlaneInitConfigSecond,
	}
	myclient := helpers.NewFakeClientWithScheme(setupScheme(), objects...)
	k := &KubeadmConfigReconciler{
		Client:          myclient,
		KubeadmInitLock: &myInitLocker{},
	}

	request := ctrl.Request{
		NamespacedName: client.ObjectKey{
			Namespace: "default",
			Name:      "control-plane-init-cfg-first",
		},
	}
	result, err := k.Reconcile(ctx, request)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.Requeue).To(BeFalse())
	g.Expect(result.RequeueAfter).To(Equal(time.Duration(0)))

	request = ctrl.Request{
		NamespacedName: client.ObjectKey{
			Namespace: "default",
			Name:      "control-plane-init-cfg-second",
		},
	}
	result, err = k.Reconcile(ctx, request)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.Requeue).To(BeFalse())
	g.Expect(result.RequeueAfter).To(Equal(30 * time.Second))
}

// Patch should be applied if there is an error in reconcile
func TestKubeadmConfigReconciler_Reconcile_PatchWhenErrorOccurred(t *testing.T) {
	g := NewWithT(t)

	cluster := newCluster("cluster")
	cluster.Status.InfrastructureReady = true

	controlPlaneInitMachine := newControlPlaneMachine(cluster, "control-plane-init-machine")
	controlPlaneInitConfig := newControlPlaneInitKubeadmConfig(controlPlaneInitMachine, "control-plane-init-cfg")

	// set InitConfiguration as nil, we will check this to determine if the kubeadm config has been patched
	controlPlaneInitConfig.Spec.InitConfiguration = nil

	objects := []client.Object{
		cluster,
		controlPlaneInitMachine,
		controlPlaneInitConfig,
	}

	secrets := createSecrets(t, cluster, controlPlaneInitConfig)
	for _, obj := range secrets {
		s := obj.(*corev1.Secret)
		delete(s.Data, secret.TLSCrtDataName) // destroy the secrets, which will cause Reconcile to fail
		objects = append(objects, s)
	}

	myclient := helpers.NewFakeClientWithScheme(setupScheme(), objects...)
	k := &KubeadmConfigReconciler{
		Client:          myclient,
		KubeadmInitLock: &myInitLocker{},
	}

	request := ctrl.Request{
		NamespacedName: client.ObjectKey{
			Namespace: "default",
			Name:      "control-plane-init-cfg",
		},
	}

	result, err := k.Reconcile(ctx, request)
	g.Expect(err).To(HaveOccurred())
	g.Expect(result.Requeue).To(BeFalse())
	g.Expect(result.RequeueAfter).To(Equal(time.Duration(0)))

	cfg, err := getKubeadmConfig(myclient, "control-plane-init-cfg")
	g.Expect(err).NotTo(HaveOccurred())
	// check if the kubeadm config has been patched
	g.Expect(cfg.Spec.InitConfiguration).ToNot(BeNil())
	g.Expect(cfg.Status.ObservedGeneration).NotTo(BeNil())
}

func TestKubeadmConfigReconciler_ResolveFiles(t *testing.T) {
	testSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "source",
		},
		Data: map[string][]byte{
			"key": []byte("foo"),
		},
	}

	cases := map[string]struct {
		cfg     *bootstrapv1.KubeadmConfig
		objects []client.Object
		expect  []bootstrapv1.File
	}{
		"content should pass through": {
			cfg: &bootstrapv1.KubeadmConfig{
				Spec: bootstrapv1.KubeadmConfigSpec{
					Files: []bootstrapv1.File{
						{
							Content:     "foo",
							Path:        "/path",
							Owner:       "root:root",
							Permissions: "0600",
						},
					},
				},
			},
			expect: []bootstrapv1.File{
				{
					Content:     "foo",
					Path:        "/path",
					Owner:       "root:root",
					Permissions: "0600",
				},
			},
		},
		"contentFrom should convert correctly": {
			cfg: &bootstrapv1.KubeadmConfig{
				Spec: bootstrapv1.KubeadmConfigSpec{
					Files: []bootstrapv1.File{
						{
							ContentFrom: &bootstrapv1.FileSource{
								Secret: bootstrapv1.SecretFileSource{
									Name: "source",
									Key:  "key",
								},
							},
							Path:        "/path",
							Owner:       "root:root",
							Permissions: "0600",
						},
					},
				},
			},
			expect: []bootstrapv1.File{
				{
					Content:     "foo",
					Path:        "/path",
					Owner:       "root:root",
					Permissions: "0600",
				},
			},
			objects: []client.Object{testSecret},
		},
		"multiple files should work correctly": {
			cfg: &bootstrapv1.KubeadmConfig{
				Spec: bootstrapv1.KubeadmConfigSpec{
					Files: []bootstrapv1.File{
						{
							Content:     "bar",
							Path:        "/bar",
							Owner:       "root:root",
							Permissions: "0600",
						},
						{
							ContentFrom: &bootstrapv1.FileSource{
								Secret: bootstrapv1.SecretFileSource{
									Name: "source",
									Key:  "key",
								},
							},
							Path:        "/path",
							Owner:       "root:root",
							Permissions: "0600",
						},
					},
				},
			},
			expect: []bootstrapv1.File{
				{
					Content:     "bar",
					Path:        "/bar",
					Owner:       "root:root",
					Permissions: "0600",
				},
				{
					Content:     "foo",
					Path:        "/path",
					Owner:       "root:root",
					Permissions: "0600",
				},
			},
			objects: []client.Object{testSecret},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)

			myclient := helpers.NewFakeClientWithScheme(setupScheme(), tc.objects...)
			k := &KubeadmConfigReconciler{
				Client:          myclient,
				KubeadmInitLock: &myInitLocker{},
			}

			// make a list of files we expect to be sourced from secrets
			// after we resolve files, assert that the original spec has
			// not been mutated and all paths we expected to be sourced
			// from secrets still are.
			contentFrom := map[string]bool{}
			for _, file := range tc.cfg.Spec.Files {
				if file.ContentFrom != nil {
					contentFrom[file.Path] = true
				}
			}

			files, err := k.resolveFiles(ctx, tc.cfg)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(files).To(Equal(tc.expect))
			for _, file := range tc.cfg.Spec.Files {
				if contentFrom[file.Path] {
					g.Expect(file.ContentFrom).NotTo(BeNil())
					g.Expect(file.Content).To(Equal(""))
				}
			}
		})
	}
}

// test utils

// newCluster return a CAPI cluster object
func newCluster(name string) *clusterv1.Cluster {
	return &clusterv1.Cluster{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Cluster",
			APIVersion: clusterv1.GroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      name,
		},
	}
}

// newMachine return a CAPI machine object; if cluster is not nil, the machine is linked to the cluster as well
func newMachine(cluster *clusterv1.Cluster, name string) *clusterv1.Machine {
	machine := &clusterv1.Machine{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Machine",
			APIVersion: clusterv1.GroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      name,
		},
		Spec: clusterv1.MachineSpec{
			Bootstrap: clusterv1.Bootstrap{
				ConfigRef: &corev1.ObjectReference{
					Kind:       "KubeadmConfig",
					APIVersion: bootstrapv1.GroupVersion.String(),
				},
			},
		},
	}
	if cluster != nil {
		machine.Spec.ClusterName = cluster.Name
		machine.ObjectMeta.Labels = map[string]string{
			clusterv1.ClusterLabelName: cluster.Name,
		}
	}
	return machine
}

func newWorkerMachine(cluster *clusterv1.Cluster) *clusterv1.Machine {
	return newMachine(cluster, "worker-machine") // machine by default is a worker node (not the bootstrapNode)
}

func newControlPlaneMachine(cluster *clusterv1.Cluster, name string) *clusterv1.Machine {
	m := newMachine(cluster, name)
	m.Labels[clusterv1.MachineControlPlaneLabelName] = ""
	return m
}

// newMachinePool return a CAPI machine pool object; if cluster is not nil, the machine pool is linked to the cluster as well
func newMachinePool(cluster *clusterv1.Cluster, name string) *expv1.MachinePool {
	machine := &expv1.MachinePool{
		TypeMeta: metav1.TypeMeta{
			Kind:       "MachinePool",
			APIVersion: expv1.GroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      name,
		},
		Spec: expv1.MachinePoolSpec{
			Template: clusterv1.MachineTemplateSpec{
				Spec: clusterv1.MachineSpec{
					Bootstrap: clusterv1.Bootstrap{
						ConfigRef: &corev1.ObjectReference{
							Kind:       "KubeadmConfig",
							APIVersion: bootstrapv1.GroupVersion.String(),
						},
					},
				},
			},
		},
	}
	if cluster != nil {
		machine.Spec.ClusterName = cluster.Name
		machine.ObjectMeta.Labels = map[string]string{
			clusterv1.ClusterLabelName: cluster.Name,
		}
	}
	return machine
}

func newWorkerMachinePool(cluster *clusterv1.Cluster) *expv1.MachinePool {
	return newMachinePool(cluster, "worker-machinepool")
}

// newKubeadmConfig return a CABPK KubeadmConfig object; if machine is not nil, the KubeadmConfig is linked to the machine as well
func newKubeadmConfig(machine *clusterv1.Machine, name string) *bootstrapv1.KubeadmConfig {
	config := &bootstrapv1.KubeadmConfig{
		TypeMeta: metav1.TypeMeta{
			Kind:       "KubeadmConfig",
			APIVersion: bootstrapv1.GroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      name,
		},
	}
	if machine != nil {
		config.ObjectMeta.OwnerReferences = []metav1.OwnerReference{
			{
				Kind:       "Machine",
				APIVersion: clusterv1.GroupVersion.String(),
				Name:       machine.Name,
				UID:        types.UID(fmt.Sprintf("%s uid", machine.Name)),
			},
		}
		machine.Spec.Bootstrap.ConfigRef.Name = config.Name
		machine.Spec.Bootstrap.ConfigRef.Namespace = config.Namespace
	}
	return config
}

func newWorkerJoinKubeadmConfig(machine *clusterv1.Machine) *bootstrapv1.KubeadmConfig {
	c := newKubeadmConfig(machine, "worker-join-cfg")
	c.Spec.JoinConfiguration = &kubeadmv1beta1.JoinConfiguration{
		ControlPlane: nil,
	}
	return c
}

func newControlPlaneJoinKubeadmConfig(machine *clusterv1.Machine, name string) *bootstrapv1.KubeadmConfig {
	c := newKubeadmConfig(machine, name)
	c.Spec.JoinConfiguration = &kubeadmv1beta1.JoinConfiguration{
		ControlPlane: &kubeadmv1beta1.JoinControlPlane{},
	}
	return c
}

func newControlPlaneInitKubeadmConfig(machine *clusterv1.Machine, name string) *bootstrapv1.KubeadmConfig {
	c := newKubeadmConfig(machine, name)
	c.Spec.ClusterConfiguration = &kubeadmv1beta1.ClusterConfiguration{}
	c.Spec.InitConfiguration = &kubeadmv1beta1.InitConfiguration{}
	return c
}

// newMachinePoolKubeadmConfig return a CABPK KubeadmConfig object; if machine pool is not nil,
// the KubeadmConfig is linked to the machine pool as well
func newMachinePoolKubeadmConfig(machinePool *expv1.MachinePool, name string) *bootstrapv1.KubeadmConfig {
	config := &bootstrapv1.KubeadmConfig{
		TypeMeta: metav1.TypeMeta{
			Kind:       "KubeadmConfig",
			APIVersion: bootstrapv1.GroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      name,
		},
	}
	if machinePool != nil {
		config.ObjectMeta.OwnerReferences = []metav1.OwnerReference{
			{
				Kind:       "MachinePool",
				APIVersion: expv1.GroupVersion.String(),
				Name:       machinePool.Name,
				UID:        types.UID(fmt.Sprintf("%s uid", machinePool.Name)),
			},
		}
		machinePool.Spec.Template.Spec.Bootstrap.ConfigRef.Name = config.Name
		machinePool.Spec.Template.Spec.Bootstrap.ConfigRef.Namespace = config.Namespace
	}
	return config
}

func newWorkerPoolJoinKubeadmConfig(machinePool *expv1.MachinePool) *bootstrapv1.KubeadmConfig {
	c := newMachinePoolKubeadmConfig(machinePool, "workerpool-join-cfg")
	c.Spec.JoinConfiguration = &kubeadmv1beta1.JoinConfiguration{
		ControlPlane: nil,
	}
	return c
}

func createSecrets(t *testing.T, cluster *clusterv1.Cluster, config *bootstrapv1.KubeadmConfig) []client.Object {
	out := []client.Object{}
	if config.Spec.ClusterConfiguration == nil {
		config.Spec.ClusterConfiguration = &kubeadmv1beta1.ClusterConfiguration{}
	}
	certificates := secret.NewCertificatesForInitialControlPlane(config.Spec.ClusterConfiguration)
	if err := certificates.Generate(); err != nil {
		t.Fatal(err)
	}
	for _, certificate := range certificates {
		out = append(out, certificate.AsSecret(util.ObjectKey(cluster), *metav1.NewControllerRef(config, bootstrapv1.GroupVersion.WithKind("KubeadmConfig"))))
	}
	return out
}

type myInitLocker struct {
	locked bool
}

func (m *myInitLocker) Lock(_ context.Context, _ *clusterv1.Cluster, _ *clusterv1.Machine) bool {
	if !m.locked {
		m.locked = true
		return true
	}
	return false
}

func (m *myInitLocker) Unlock(_ context.Context, _ *clusterv1.Cluster) bool {
	if m.locked {
		m.locked = false
	}
	return true
}

func assertHasFalseCondition(g *WithT, myclient client.Client, req ctrl.Request, t clusterv1.ConditionType, s clusterv1.ConditionSeverity, r string) {
	config := &bootstrapv1.KubeadmConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: req.Namespace,
		},
	}

	configKey := client.ObjectKeyFromObject(config)
	g.Expect(myclient.Get(ctx, configKey, config)).To(Succeed())
	c := conditions.Get(config, t)
	g.Expect(c).ToNot(BeNil())
	g.Expect(c.Status).To(Equal(corev1.ConditionFalse))
	g.Expect(c.Severity).To(Equal(s))
	g.Expect(c.Reason).To(Equal(r))
}

func assertHasTrueCondition(g *WithT, myclient client.Client, req ctrl.Request, t clusterv1.ConditionType) {
	config := &bootstrapv1.KubeadmConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: req.Namespace,
		},
	}

	configKey := client.ObjectKeyFromObject(config)
	g.Expect(myclient.Get(ctx, configKey, config)).To(Succeed())
	c := conditions.Get(config, t)
	g.Expect(c).ToNot(BeNil())
	g.Expect(c.Status).To(Equal(corev1.ConditionTrue))
}
