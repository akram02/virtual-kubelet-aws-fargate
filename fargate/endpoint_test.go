package fargate

import (
	"context"
	"fmt"
	"io/ioutil"
	"strings"
	"testing"

	"github.com/virtual-kubelet/aws-fargate/internal/fakeaws"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	"gotest.tools/assert"
	is "gotest.tools/assert/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	exampleAccount       = fakeaws.Account
	exampleSubnet        = fakeaws.Subnet
	exampleSecurityGroup = fakeaws.SecurityGroup
	exampleRegion        = fakeaws.Region
)

func testClusterConfig() *ClusterConfig {
	return &ClusterConfig{
		Region:          exampleRegion,
		Name:            "test",
		Subnets:         []string{exampleSubnet},
		SecurityGroups:  []string{exampleSecurityGroup},
		PlatformVersion: "LATEST",
	}
}

// TestClientUsesEndpointURLAndAnyRegion: the client reaches the endpoint AWS_ENDPOINT_URL names, signs for the Region
// it was given whatever its name, and NewCluster creates a cluster that does not exist.
func TestClientUsesEndpointURLAndAnyRegion(t *testing.T) {
	fakeaws.Isolate(t)
	f := fakeaws.New(t)
	f.AnswerNewCluster()
	t.Setenv("AWS_ENDPOINT_URL", f.Server.URL)

	cluster, err := NewCluster(context.Background(), testClusterConfig())
	assert.NilError(t, err)

	assert.DeepEqual(t, f.Targets(), []string{"DescribeClusters", "CreateCluster", "ListTasks"})
	created := f.Last("CreateCluster")
	assert.Check(t, is.Equal(created.Body["clusterName"], "test"))
	assert.Check(t, is.Equal(created.Region, exampleRegion))
	assert.Check(t, is.Equal(created.Signer, "ecs"))
	assert.Check(t, is.Equal(cluster.arn, fakeaws.ClusterArn("test")))
	assert.Check(t, is.Equal(cluster.region, exampleRegion))
}

// TestClientRegionFromEnvironment: with no Region in the provider configuration, the SDK's own Region is used.
func TestClientRegionFromEnvironment(t *testing.T) {
	fakeaws.Isolate(t)
	f := fakeaws.New(t)
	f.AnswerNewCluster()
	t.Setenv("AWS_ENDPOINT_URL", f.Server.URL)
	t.Setenv("AWS_REGION", exampleRegion)

	config := testClusterConfig()
	config.Region = ""
	_, err := NewCluster(context.Background(), config)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(f.Last("CreateCluster").Region, exampleRegion))
}

// TestClientNeedsARegion: with no Region anywhere, NewCluster refuses before any call.
func TestClientNeedsARegion(t *testing.T) {
	fakeaws.Isolate(t)
	f := fakeaws.New(t)
	f.AnswerNewCluster()
	t.Setenv("AWS_ENDPOINT_URL", f.Server.URL)

	config := testClusterConfig()
	config.Region = ""
	_, err := NewCluster(context.Background(), config)
	assert.ErrorContains(t, err, "no AWS Region is set")
	assert.Check(t, is.Len(f.Targets(), 0))
}

// TestServiceSpecificEndpoints: AWS_ENDPOINT_URL_ECS and AWS_ENDPOINT_URL_CLOUDWATCH_LOGS each send their service to
// their own endpoint, and container logs are read from the CloudWatch Logs one.
func TestServiceSpecificEndpoints(t *testing.T) {
	fakeaws.Isolate(t)
	ecsEndpoint := fakeaws.New(t)
	ecsEndpoint.AnswerNewCluster()
	logsEndpoint := fakeaws.New(t)
	logsEndpoint.Answer("DescribeLogStreams", func(body map[string]interface{}) (int, interface{}) {
		return 200, map[string]interface{}{"logStreams": []interface{}{
			map[string]interface{}{"logStreamName": body["logStreamNamePrefix"].(string) + "/web/0123456789abcdef0123456789abcdef"}}}
	})
	logsEndpoint.Answer("GetLogEvents", func(body map[string]interface{}) (int, interface{}) {
		if body["nextToken"] != nil {
			return 200, map[string]interface{}{"events": []interface{}{}, "nextForwardToken": body["nextToken"]}
		}
		return 200, map[string]interface{}{"events": []interface{}{
			map[string]interface{}{"timestamp": 1, "message": "first line"},
			map[string]interface{}{"timestamp": 2, "message": "second line"},
		}, "nextForwardToken": "f/1"}
	})
	t.Setenv("AWS_ENDPOINT_URL_ECS", ecsEndpoint.Server.URL)
	t.Setenv("AWS_ENDPOINT_URL_CLOUDWATCH_LOGS", logsEndpoint.Server.URL)

	config := testClusterConfig()
	config.CloudWatchLogGroupName = "/ecs/example"
	cluster, err := NewCluster(context.Background(), config)
	assert.NilError(t, err)

	reader, err := cluster.GetContainerLogs(context.Background(), "default", "p1", "web", api.ContainerLogOpts{Tail: 10})
	assert.NilError(t, err)
	logs, err := ioutil.ReadAll(reader)
	assert.NilError(t, err)

	assert.Check(t, is.Equal(string(logs), "first line\nsecond line\n"))
	assert.DeepEqual(t, logsEndpoint.Targets(), []string{"DescribeLogStreams", "GetLogEvents", "GetLogEvents"})
	assert.Check(t, is.Equal(logsEndpoint.Last("DescribeLogStreams").Signer, "logs"))
	assert.Check(t, is.Equal(logsEndpoint.Last("DescribeLogStreams").Body["logGroupName"], "/ecs/example"))
	assert.Check(t, is.Equal(logsEndpoint.Last("DescribeLogStreams").Body["logStreamNamePrefix"], "vk-podspec_test_default_p1_web"))
	for _, target := range ecsEndpoint.Targets() {
		assert.Check(t, !strings.Contains(target, "Log"), "a CloudWatch Logs call reached the ECS endpoint: %s", target)
	}
}

func examplePod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default", UID: "8d1f6b51-0d2a-4b5e-9a35-6f2a1d0c7e11"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:    "web",
			Image:   "public.ecr.aws/docker/library/busybox:latest",
			Command: []string{"sh", "-c"},
			Args:    []string{"sleep 3600"},
			Env:     []corev1.EnvVar{{Name: "GREETING", Value: "hello"}},
		}}},
	}
}

const exampleTaskArn = "arn:aws:ecs:" + exampleRegion + ":" + exampleAccount + ":task/test/0123456789abcdef0123456789abcdef"

// TestPodLifecycleRequests: the requests a pod's start, status and stop send, and how their answers are read.
func TestPodLifecycleRequests(t *testing.T) {
	fakeaws.Isolate(t)
	f := fakeaws.New(t)
	f.AnswerNewCluster()
	family := "vk-podspec_test_default_p1"
	taskDefinitionArn := fmt.Sprintf("arn:aws:ecs:%s:%s:task-definition/%s:1", exampleRegion, exampleAccount, family)
	f.Answer("RegisterTaskDefinition", func(body map[string]interface{}) (int, interface{}) {
		return 200, map[string]interface{}{"taskDefinition": map[string]interface{}{
			"taskDefinitionArn": taskDefinitionArn, "family": body["family"], "revision": 1, "status": "ACTIVE"}}
	})
	f.Answer("RunTask", func(body map[string]interface{}) (int, interface{}) {
		return 200, map[string]interface{}{"failures": []interface{}{}, "tasks": []interface{}{map[string]interface{}{
			"taskArn": exampleTaskArn, "lastStatus": "PROVISIONING", "taskDefinitionArn": taskDefinitionArn}}}
	})
	f.Answer("DescribeTasks", func(body map[string]interface{}) (int, interface{}) {
		return 200, map[string]interface{}{"failures": []interface{}{}, "tasks": []interface{}{map[string]interface{}{
			"taskArn": exampleTaskArn, "lastStatus": "RUNNING", "createdAt": 1.7e9, "taskDefinitionArn": taskDefinitionArn,
			"attachments": []interface{}{map[string]interface{}{"type": "ElasticNetworkInterface", "status": "ATTACHED",
				"details": []interface{}{
					map[string]interface{}{"name": "subnetId", "value": exampleSubnet},
					map[string]interface{}{"name": "privateIPv4Address", "value": "10.0.0.10"},
				}}},
			"containers": []interface{}{map[string]interface{}{"name": "web", "lastStatus": "RUNNING"}},
		}}}
	})
	f.Answer("StopTask", func(body map[string]interface{}) (int, interface{}) {
		return 200, map[string]interface{}{"task": map[string]interface{}{"taskArn": exampleTaskArn, "desiredStatus": "STOPPED"}}
	})
	f.Answer("DeregisterTaskDefinition", func(body map[string]interface{}) (int, interface{}) {
		return 200, map[string]interface{}{"taskDefinition": map[string]interface{}{"taskDefinitionArn": taskDefinitionArn, "status": "INACTIVE"}}
	})
	t.Setenv("AWS_ENDPOINT_URL", f.Server.URL)
	ctx := context.Background()

	cluster, err := NewCluster(ctx, testClusterConfig())
	assert.NilError(t, err)
	pod, err := NewPod(ctx, cluster, examplePod())
	assert.NilError(t, err)

	registered := f.Last("RegisterTaskDefinition").Body
	assert.Check(t, is.Equal(registered["family"], family))
	assert.Check(t, is.Equal(registered["networkMode"], "awsvpc"))
	assert.Check(t, is.DeepEqual(registered["requiresCompatibilities"], []interface{}{"FARGATE"}))
	assert.Check(t, is.Equal(registered["cpu"], "256"))
	assert.Check(t, is.Equal(registered["memory"], "512"))
	containers := registered["containerDefinitions"].([]interface{})
	assert.Assert(t, is.Len(containers, 1))
	web := containers[0].(map[string]interface{})
	assert.Check(t, is.Equal(web["name"], "web"))
	assert.Check(t, is.Equal(web["image"], "public.ecr.aws/docker/library/busybox:latest"))
	assert.Check(t, is.Equal(web["cpu"], float64(256)))
	assert.Check(t, is.Equal(web["memory"], float64(512)))
	assert.Check(t, is.Equal(web["memoryReservation"], float64(512)))
	assert.Check(t, is.DeepEqual(web["entryPoint"], []interface{}{"sh", "-c"}))
	assert.Check(t, is.DeepEqual(web["command"], []interface{}{"sleep 3600"}))
	assert.Check(t, is.DeepEqual(web["environment"], []interface{}{map[string]interface{}{"name": "GREETING", "value": "hello"}}))

	assert.NilError(t, pod.Start(ctx))
	run := f.Last("RunTask").Body
	assert.Check(t, is.Equal(run["cluster"], "test"))
	assert.Check(t, is.Equal(run["launchType"], "FARGATE"))
	assert.Check(t, is.Equal(run["count"], float64(1)))
	assert.Check(t, is.Equal(run["taskDefinition"], taskDefinitionArn))
	assert.Check(t, is.Equal(run["startedBy"], "8d1f6b51-0d2a-4b5e-9a35-6f2a1d0c7e11"))
	assert.Check(t, is.Equal(run["platformVersion"], "LATEST"))
	assert.Check(t, is.DeepEqual(run["networkConfiguration"], map[string]interface{}{"awsvpcConfiguration": map[string]interface{}{
		"subnets": []interface{}{exampleSubnet}, "securityGroups": []interface{}{exampleSecurityGroup}, "assignPublicIp": "DISABLED"}}))

	status := pod.GetStatus(ctx)
	assert.Check(t, is.Equal(status.Phase, corev1.PodRunning))
	assert.Check(t, is.Equal(status.PodIP, "10.0.0.10"))
	assert.Assert(t, is.Len(status.ContainerStatuses, 1))
	assert.Check(t, status.ContainerStatuses[0].State.Running != nil)

	assert.NilError(t, pod.Stop(ctx))
	stop := f.Last("StopTask").Body
	assert.Check(t, is.Equal(stop["task"], exampleTaskArn))
	assert.Check(t, is.Equal(stop["cluster"], "test"))
	assert.Check(t, is.Equal(f.Last("DeregisterTaskDefinition").Body["taskDefinition"], taskDefinitionArn))
	_, err = cluster.GetPod("default", "p1")
	assert.Check(t, err != nil, "the stopped pod is still in the cluster")
}

// TestErrorAnswersAreErrors: an error answer to each call is returned as an error, never read as an answer.
func TestErrorAnswersAreErrors(t *testing.T) {
	serverError := func(body map[string]interface{}) (int, interface{}) {
		return 500, map[string]string{"__type": "ServerException", "message": "The service is unavailable."}
	}
	ctx := context.Background()

	t.Run("DescribeClusters", func(t *testing.T) {
		fakeaws.Isolate(t)
		f := fakeaws.New(t)
		f.AnswerNewCluster()
		f.Answer("DescribeClusters", serverError)
		t.Setenv("AWS_ENDPOINT_URL", f.Server.URL)

		_, err := NewCluster(ctx, testClusterConfig())
		assert.ErrorContains(t, err, "ServerException")
		assert.DeepEqual(t, f.Targets(), []string{"DescribeClusters"})
	})

	t.Run("RunTask and DescribeTasks", func(t *testing.T) {
		fakeaws.Isolate(t)
		f := fakeaws.New(t)
		f.AnswerNewCluster()
		f.Answer("RegisterTaskDefinition", func(body map[string]interface{}) (int, interface{}) {
			return 200, map[string]interface{}{"taskDefinition": map[string]interface{}{"taskDefinitionArn": "arn:aws:ecs:" +
				exampleRegion + ":" + exampleAccount + ":task-definition/x:1"}}
		})
		f.Answer("RunTask", serverError)
		f.Answer("DescribeTasks", serverError)
		t.Setenv("AWS_ENDPOINT_URL", f.Server.URL)

		cluster, err := NewCluster(ctx, testClusterConfig())
		assert.NilError(t, err)
		pod, err := NewPod(ctx, cluster, examplePod())
		assert.NilError(t, err)
		assert.ErrorContains(t, pod.Start(ctx), "ServerException")
		pod.taskArn = exampleTaskArn
		assert.Check(t, is.Equal(pod.GetStatus(ctx).Phase, corev1.PodUnknown))
	})

	t.Run("RunTask failures", func(t *testing.T) {
		fakeaws.Isolate(t)
		f := fakeaws.New(t)
		f.AnswerNewCluster()
		f.Answer("RegisterTaskDefinition", func(body map[string]interface{}) (int, interface{}) {
			return 200, map[string]interface{}{"taskDefinition": map[string]interface{}{"taskDefinitionArn": "arn:aws:ecs:" +
				exampleRegion + ":" + exampleAccount + ":task-definition/x:1"}}
		})
		f.Answer("RunTask", func(body map[string]interface{}) (int, interface{}) {
			return 200, map[string]interface{}{"tasks": []interface{}{}, "failures": []interface{}{
				map[string]interface{}{"reason": "RESOURCE:MEMORY"}}}
		})
		t.Setenv("AWS_ENDPOINT_URL", f.Server.URL)

		cluster, err := NewCluster(ctx, testClusterConfig())
		assert.NilError(t, err)
		pod, err := NewPod(ctx, cluster, examplePod())
		assert.NilError(t, err)
		assert.ErrorContains(t, pod.Start(ctx), "RESOURCE:MEMORY")
	})
}
