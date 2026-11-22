package fargate

import (
	"context"
	"io/ioutil"
	"testing"

	"github.com/virtual-kubelet/aws-fargate/internal/fakeaws"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	"gotest.tools/assert"
	is "gotest.tools/assert/cmp"
)

// logsEndpoints answers a container's log stream and one page of two events, and returns the CloudWatch Logs endpoint.
func logsEndpoints(t *testing.T) (*Cluster, *fakeaws.Endpoint) {
	fakeaws.Isolate(t)
	ecsEndpoint := fakeaws.New(t)
	ecsEndpoint.AnswerNewCluster()
	logsEndpoint := fakeaws.New(t)
	logsEndpoint.Answer("DescribeLogStreams", func(body map[string]interface{}) (int, interface{}) {
		return 200, map[string]interface{}{"logStreams": []interface{}{
			map[string]interface{}{"logStreamName": body["logStreamNamePrefix"].(string) + "/web/0123456789abcdef0123456789abcdef"}}}
	})
	logsEndpoint.Answer("GetLogEvents", func(body map[string]interface{}) (int, interface{}) {
		if limit, ok := body["limit"]; ok && limit.(float64) < 1 {
			return 400, map[string]string{"__type": "InvalidParameterException",
				"message": "1 validation error detected: Value '0' at 'limit' failed to satisfy constraint: Member must have value greater than or equal to 1"}
		}
		if body["nextToken"] != nil { // the page after the last: no events, the same token, as CloudWatch Logs answers it
			return 200, map[string]interface{}{"events": []interface{}{}, "nextForwardToken": body["nextToken"]}
		}
		return 200, map[string]interface{}{"events": []interface{}{map[string]interface{}{"timestamp": 1, "message": "a line"}}, "nextForwardToken": "f/1"}
	})
	t.Setenv("AWS_ENDPOINT_URL_ECS", ecsEndpoint.Server.URL)
	t.Setenv("AWS_ENDPOINT_URL_CLOUDWATCH_LOGS", logsEndpoint.Server.URL)
	config := testClusterConfig()
	config.CloudWatchLogGroupName = "/ecs/example"
	cluster, err := NewCluster(context.Background(), config)
	assert.NilError(t, err)
	return cluster, logsEndpoint
}

// TestALogReadWithoutATailAsksForNoLimit: a plain `kubectl logs` is a tail of 0, every line. GetLogEvents refuses a
// limit of 0, so the request carries none; a tail of 3 is a limit of 3.
func TestALogReadWithoutATailAsksForNoLimit(t *testing.T) {
	cluster, logsEndpoint := logsEndpoints(t)
	reader, err := cluster.GetContainerLogs(context.Background(), "default", "p1", "web", api.ContainerLogOpts{})
	assert.NilError(t, err)
	logs, err := ioutil.ReadAll(reader)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(string(logs), "a line\n"))
	_, sent := logsEndpoint.Last("GetLogEvents").Body["limit"]
	assert.Check(t, !sent, "a tail of 0 sent a limit: %v", logsEndpoint.Last("GetLogEvents").Body)

	_, err = cluster.GetContainerLogs(context.Background(), "default", "p1", "web", api.ContainerLogOpts{Tail: 3})
	assert.NilError(t, err)
	assert.Check(t, is.Equal(logsEndpoint.Last("GetLogEvents").Body["limit"], float64(3)))
}

