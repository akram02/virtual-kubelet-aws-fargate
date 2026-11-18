// Copyright © 2017 The virtual-kubelet authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/virtual-kubelet/aws-fargate/internal/fakeaws"
	"gotest.tools/assert"
	is "gotest.tools/assert/cmp"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// startAPIServer starts a kube-apiserver and etcd (no scheduler, no controllers, no kubelet) for the test. The test is
// skipped unless KUBEBUILDER_ASSETS names a directory holding the kube-apiserver and etcd binaries, as
// `setup-envtest use -p path` prints.
func startAPIServer(t *testing.T) kubernetes.Interface {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set: it must name a directory holding kube-apiserver and etcd")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	assert.NilError(t, err)
	t.Cleanup(func() { _ = env.Stop() })
	client, err := kubernetes.NewForConfig(cfg)
	assert.NilError(t, err)
	return client
}

func eventually(t *testing.T, what string, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestNodeRunsPodsAsFargateTasks runs the node as the command builds it, against a real API server and a fake ECS: the
// node registers with its taint, labels, capacity and version and becomes Ready; a pod bound to it is started as a task
// of the configured cluster, subnet and security group; and deleting the pod stops the task.
func TestNodeRunsPodsAsFargateTasks(t *testing.T) {
	client := startAPIServer(t)
	fakeaws.Isolate(t)
	ecs := fakeaws.NewECS(t)
	t.Setenv("AWS_ENDPOINT_URL", ecs.Server.URL)

	providerConfig := filepath.Join(t.TempDir(), "fargate.toml")
	assert.NilError(t, os.WriteFile(providerConfig, []byte(fmt.Sprintf(`
Region = "%s"
ClusterName = "test"
Subnets = ["%s"]
SecurityGroups = ["%s"]
`, fakeaws.Region, fakeaws.Subnet, fakeaws.SecurityGroup)), 0o600))

	o := defaultOpts()
	o.nodeName = "vk-test"
	o.providerConfig = providerConfig
	o.internalIP = "10.0.0.1"
	o.certPath, o.keyPath = "", ""

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	vk, err := newNode(ctx, o, client)
	assert.NilError(t, err)
	go func() { _ = vk.Run(ctx) }()
	assert.NilError(t, vk.WaitReady(ctx, time.Minute))

	// The node.
	node, err := client.CoreV1().Nodes().Get(ctx, "vk-test", metav1.GetOptions{})
	assert.NilError(t, err)
	ourTaint := corev1.Taint{Key: "virtual-kubelet.io/provider", Value: "aws", Effect: corev1.TaintEffectNoSchedule}
	hasTaint := false
	for _, taint := range node.Spec.Taints {
		hasTaint = hasTaint || (taint.Key == ourTaint.Key && taint.Value == ourTaint.Value && taint.Effect == ourTaint.Effect)
	}
	assert.Check(t, hasTaint, "the node's taints are %+v", node.Spec.Taints)
	assert.Check(t, is.Equal(node.Labels[corev1.LabelOSStable], "linux"))
	assert.Check(t, is.Equal(node.Labels["type"], "virtual-kubelet"))
	cpu := node.Status.Capacity[corev1.ResourceCPU]
	assert.Check(t, is.Equal(cpu.String(), "20"))
	pods := node.Status.Capacity[corev1.ResourcePods]
	assert.Check(t, is.Equal(pods.String(), "20"))
	assert.Check(t, strings.Contains(node.Status.NodeInfo.KubeletVersion, "vk-aws-fargate"), node.Status.NodeInfo.KubeletVersion)
	assert.Check(t, is.DeepEqual(node.Status.Addresses, []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.1"}}))
	eventually(t, "the node to be Ready", 30*time.Second, func() bool {
		node, err := client.CoreV1().Nodes().Get(ctx, "vk-test", metav1.GetOptions{})
		if err != nil {
			return false
		}
		for _, condition := range node.Status.Conditions {
			if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
				return true
			}
		}
		return false
	})
	assert.Check(t, is.Equal(ecs.Last("CreateCluster").Body["clusterName"], "test"))

	// A pod bound to the node is started as a task.
	automount := false
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName:                     "vk-test",
			AutomountServiceAccountToken: &automount,
			Tolerations:                  []corev1.Toleration{{Key: "virtual-kubelet.io/provider", Operator: corev1.TolerationOpExists}},
			Containers: []corev1.Container{{
				Name:  "web",
				Image: "public.ecr.aws/docker/library/busybox:latest",
				Env:   []corev1.EnvVar{{Name: "GREETING", Value: "hello"}},
			}},
		},
	}
	pod, err = client.CoreV1().Pods("default").Create(ctx, pod, metav1.CreateOptions{})
	assert.NilError(t, err)

	eventually(t, "RunTask", 30*time.Second, func() bool { return ecs.Count("RunTask") >= 1 })
	run := ecs.Last("RunTask").Body
	assert.Check(t, is.Equal(run["cluster"], "test"))
	assert.Check(t, is.Equal(run["startedBy"], string(pod.UID)))
	assert.Check(t, is.DeepEqual(run["networkConfiguration"], map[string]interface{}{"awsvpcConfiguration": map[string]interface{}{
		"subnets": []interface{}{fakeaws.Subnet}, "securityGroups": []interface{}{fakeaws.SecurityGroup}, "assignPublicIp": "DISABLED"}}))
	registered := ecs.Last("RegisterTaskDefinition").Body
	assert.Check(t, is.Equal(registered["family"], "vk-podspec_test_default_p1"))
	container := registered["containerDefinitions"].([]interface{})[0].(map[string]interface{})
	assert.Check(t, is.Equal(container["image"], "public.ecr.aws/docker/library/busybox:latest"))
	// Virtual Kubelet adds the KUBERNETES_SERVICE_* variables a kubelet adds; the pod's own variable is among them.
	assert.Check(t, is.Contains(container["environment"], map[string]interface{}{"name": "GREETING", "value": "hello"}))
	tasks := ecs.Tasks()
	assert.Assert(t, is.Len(tasks, 1))
	taskArn := fmt.Sprint(tasks[0]["taskArn"])

	// Deleting the pod stops its task.
	assert.NilError(t, client.CoreV1().Pods("default").Delete(ctx, "p1", metav1.DeleteOptions{}))
	eventually(t, "StopTask", 30*time.Second, func() bool { return ecs.Count("StopTask") >= 1 })
	assert.Check(t, is.Equal(ecs.Last("StopTask").Body["task"], taskArn))
	assert.Check(t, is.Equal(ecs.Task(taskArn)["lastStatus"], "STOPPED"))
	eventually(t, "the pod to be deleted", 30*time.Second, func() bool {
		_, err := client.CoreV1().Pods("default").Get(ctx, "p1", metav1.GetOptions{})
		return apierrors.IsNotFound(err)
	})
}

// TestNewNodeNeedsProviderConfig: the command refuses to start without a provider configuration file.
func TestNewNodeNeedsProviderConfig(t *testing.T) {
	o := defaultOpts()
	o.providerConfig = ""
	_, err := newNode(context.Background(), o, nil)
	assert.ErrorContains(t, err, "--provider-config is required")
}

// TestNewNodeRefusesWebhookAuthWithoutTLS: webhook authentication needs the kubelet API's certificate and key.
func TestNewNodeRefusesWebhookAuthWithoutTLS(t *testing.T) {
	o := defaultOpts()
	o.providerConfig = "unused.toml"
	o.certPath, o.keyPath = "", ""
	o.webhookAuth = true
	_, err := newNode(context.Background(), o, nil)
	assert.ErrorContains(t, err, "--authentication-token-webhook needs APISERVER_CERT_LOCATION")
}

// TestDeprecatedFlagsAreAccepted: the flags of earlier releases that no longer do anything are still accepted, so that
// existing deployments keep starting.
func TestDeprecatedFlagsAreAccepted(t *testing.T) {
	o := defaultOpts()
	cmd := &cobra.Command{Use: "virtual-kubelet"}
	installFlags(cmd, o)
	err := cmd.ParseFlags([]string{"--provider", "aws", "--provider-config", "fargate.toml", "--namespace", "default",
		"--metrics-addr", ":10255", "--enable-node-lease", "--taint", "example.com/key", "--trace-exporter", "jaeger",
		"--trace-sample-rate", "1", "--trace-service-name", "vk", "--trace-tag", "a=b"})
	assert.NilError(t, err)
	assert.Check(t, is.Equal(o.providerConfig, "fargate.toml"))
	assert.Check(t, is.Equal(o.taintKey, "example.com/key"))
}

// TestKubeletPortSetsTheListenAddress: KUBELET_PORT, as earlier releases read it, sets the kubelet API's port.
func TestKubeletPortSetsTheListenAddress(t *testing.T) {
	t.Setenv("KUBELET_PORT", "10260")
	assert.Check(t, is.Equal(defaultOpts().listenAddress, ":10260"))
}
