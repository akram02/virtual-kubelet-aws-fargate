package fargate

import (
	"context"
	"testing"

	"github.com/virtual-kubelet/aws-fargate/internal/fakeaws"
	"gotest.tools/assert"
	is "gotest.tools/assert/cmp"
	corev1 "k8s.io/api/core/v1"
)

// TestAnEmptyDirIsABindVolume: an emptyDir volume is a Fargate bind volume (a name, no host path) mounted where each of
// the pod's containers mounts it; a ConfigMap volume is still not mapped.
func TestAnEmptyDirIsABindVolume(t *testing.T) {
	fakeaws.Isolate(t)
	f := fakeaws.NewECS(t)
	t.Setenv("AWS_ENDPOINT_URL", f.Server.URL)
	cluster, err := NewCluster(context.Background(), testClusterConfig())
	assert.NilError(t, err)
	pod := examplePod()
	pod.Spec.Volumes = []corev1.Volume{
		{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "settings", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "cm1"}}}},
	}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "scratch", MountPath: "/ed"}, {Name: "settings", MountPath: "/cm"}}
	_, err = NewPod(context.Background(), cluster, pod)
	assert.NilError(t, err)

	registered := f.Last("RegisterTaskDefinition").Body
	assert.Check(t, is.DeepEqual(registered["volumes"], []interface{}{map[string]interface{}{"name": "scratch"}}))
	web := registered["containerDefinitions"].([]interface{})[0].(map[string]interface{})
	assert.Check(t, is.DeepEqual(web["mountPoints"], []interface{}{
		map[string]interface{}{"sourceVolume": "scratch", "containerPath": "/ed", "readOnly": false}}))
}

