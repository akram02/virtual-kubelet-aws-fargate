package fargate

import (
	"context"
	"testing"

	"github.com/virtual-kubelet/aws-fargate/internal/fakeaws"
	"gotest.tools/assert"
	is "gotest.tools/assert/cmp"
	corev1 "k8s.io/api/core/v1"
)

// TestTheStatusKeepsThePodsQOSClass: a pod's status carries the QoS class the API server gave it, never one of the
// provider's own.
func TestTheStatusKeepsThePodsQOSClass(t *testing.T) {
	fakeaws.Isolate(t)
	f := fakeaws.NewECS(t)
	t.Setenv("AWS_ENDPOINT_URL", f.Server.URL)
	cluster, err := NewCluster(context.Background(), testClusterConfig())
	assert.NilError(t, err)
	pod := examplePod()
	pod.Status.QOSClass = corev1.PodQOSGuaranteed
	fgPod, err := NewPod(context.Background(), cluster, pod)
	assert.NilError(t, err)
	assert.NilError(t, fgPod.Start(context.Background()))
	assert.Check(t, is.Equal(fgPod.GetStatus(context.Background()).QOSClass, corev1.PodQOSGuaranteed))
}
