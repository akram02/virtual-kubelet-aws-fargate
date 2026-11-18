// Package fakeaws is an in-process stand-in for the ECS and CloudWatch Logs JSON APIs, for tests. The AWS SDK reaches it
// through AWS_ENDPOINT_URL or AWS_ENDPOINT_URL_<SERVICE>, as a user points the SDK at any other endpoint. It uses only
// AWS's documentation example values.
package fakeaws

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// AWS's documentation examples.
const (
	AccessKeyID     = "AKIAIOSFODNN7EXAMPLE"
	SecretAccessKey = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	Account         = "123456789012"
	Subnet          = "subnet-12345678"
	SecurityGroup   = "sg-12345678"
	// Region is a Region name no SDK release ships a table entry for, as a private or a newer endpoint can have.
	Region = "example-region-1"
	// TaskIPv4Address is the private address the stateful ECS gives every task.
	TaskIPv4Address = "10.0.0.10"
)

// Call is one request an endpoint received.
type Call struct {
	Target string                 // the X-Amz-Target operation, without the service prefix
	Region string                 // the Region of the request's SigV4 credential scope
	Signer string                 // the service of the request's SigV4 credential scope
	Body   map[string]interface{} // the JSON request
}

// Answer answers one operation with a status code and a JSON document.
type Answer func(body map[string]interface{}) (int, interface{})

// Endpoint answers AWS JSON 1.1 requests from a table of operations and records each request.
type Endpoint struct {
	sync.Mutex
	Server  *httptest.Server
	calls   []Call
	answers map[string]Answer
}

var credentialScope = regexp.MustCompile(`Credential=[^/]+/[0-9]{8}/([^/]+)/([^/]+)/aws4_request`)

// New starts an endpoint that the test closes when it ends.
func New(t testing.TB) *Endpoint {
	e := &Endpoint{answers: map[string]Answer{}}
	e.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := r.Header.Get("X-Amz-Target")
		target = target[strings.LastIndex(target, ".")+1:]
		raw, _ := io.ReadAll(r.Body)
		body := map[string]interface{}{}
		_ = json.Unmarshal(raw, &body)
		c := Call{Target: target, Body: body}
		if m := credentialScope.FindStringSubmatch(r.Header.Get("Authorization")); m != nil {
			c.Region, c.Signer = m[1], m[2]
		}
		e.Lock()
		e.calls = append(e.calls, c)
		answer, ok := e.answers[target]
		e.Unlock()

		status, document := http.StatusBadRequest, interface{}(map[string]string{
			"__type": "UnknownOperationException", "message": "no answer for " + target})
		if ok {
			status, document = answer(body)
		}
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(document)
	}))
	t.Cleanup(e.Server.Close)
	return e
}

// Answer sets how an operation is answered.
func (e *Endpoint) Answer(target string, answer Answer) {
	e.Lock()
	defer e.Unlock()
	e.answers[target] = answer
}

// Targets returns the operations received, in order.
func (e *Endpoint) Targets() []string {
	e.Lock()
	defer e.Unlock()
	targets := []string{}
	for _, c := range e.calls {
		targets = append(targets, c.Target)
	}
	return targets
}

// Last returns the last call of an operation, or an empty Call.
func (e *Endpoint) Last(target string) Call {
	e.Lock()
	defer e.Unlock()
	for i := len(e.calls) - 1; i >= 0; i-- {
		if e.calls[i].Target == target {
			return e.calls[i]
		}
	}
	return Call{}
}

// Count returns how many calls of an operation were received.
func (e *Endpoint) Count(target string) int {
	e.Lock()
	defer e.Unlock()
	n := 0
	for _, c := range e.calls {
		if c.Target == target {
			n++
		}
	}
	return n
}

// Isolate gives a test the documentation example credentials and nothing else of the environment it runs in: no
// profile, no shared files, no instance metadata, no Region, no endpoint, and one attempt per call.
func Isolate(t testing.TB) {
	none := filepath.Join(t.TempDir(), "none")
	t.Setenv("AWS_ACCESS_KEY_ID", AccessKeyID)
	t.Setenv("AWS_SECRET_ACCESS_KEY", SecretAccessKey)
	t.Setenv("AWS_CONFIG_FILE", none)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", none)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
	for _, name := range []string{"AWS_SESSION_TOKEN", "AWS_PROFILE", "AWS_DEFAULT_PROFILE", "AWS_REGION", "AWS_DEFAULT_REGION",
		"AWS_ENDPOINT_URL", "AWS_ENDPOINT_URL_ECS", "AWS_ENDPOINT_URL_CLOUDWATCH_LOGS", "AWS_IGNORE_CONFIGURED_ENDPOINT_URLS",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_CONTAINER_CREDENTIALS_FULL_URI", "AWS_WEB_IDENTITY_TOKEN_FILE"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
}

// ClusterArn returns the example ARN of a cluster.
func ClusterArn(name string) string {
	return fmt.Sprintf("arn:aws:ecs:%s:%s:cluster/%s", Region, Account, name)
}

// AnswerNewCluster answers the three calls a provider makes for a cluster that does not exist yet.
func (e *Endpoint) AnswerNewCluster() {
	e.Answer("DescribeClusters", func(body map[string]interface{}) (int, interface{}) {
		return 200, map[string]interface{}{"clusters": []interface{}{}, "failures": []interface{}{
			map[string]interface{}{"arn": ClusterArn("test"), "reason": "MISSING"}}}
	})
	e.Answer("CreateCluster", func(body map[string]interface{}) (int, interface{}) {
		return 200, map[string]interface{}{"cluster": map[string]interface{}{
			"clusterArn": ClusterArn(fmt.Sprint(body["clusterName"])), "clusterName": body["clusterName"], "status": "ACTIVE"}}
	})
	e.Answer("ListTasks", func(body map[string]interface{}) (int, interface{}) {
		return 200, map[string]interface{}{"taskArns": []interface{}{}}
	})
}

// ECS is a small stateful ECS for one cluster: task definitions are registered and deregistered, a task started by
// RunTask is RUNNING at once with TaskIPv4Address, and StopTask stops it.
type ECS struct {
	*Endpoint
	mu          sync.Mutex
	definitions map[string]map[string]interface{} // by ARN
	tasks       map[string]map[string]interface{} // by ARN
	n           int
}

// NewECS starts a stateful ECS endpoint.
func NewECS(t testing.TB) *ECS {
	s := &ECS{Endpoint: New(t), definitions: map[string]map[string]interface{}{}, tasks: map[string]map[string]interface{}{}}
	s.AnswerNewCluster()
	s.Answer("RegisterTaskDefinition", s.register)
	s.Answer("DescribeTaskDefinition", s.describeTaskDefinition)
	s.Answer("DeregisterTaskDefinition", s.deregister)
	s.Answer("RunTask", s.run)
	s.Answer("DescribeTasks", s.describeTasks)
	s.Answer("ListTasks", s.list)
	s.Answer("StopTask", s.stop)
	return s
}

// Task returns a copy of the task document with the given ARN, or nil.
func (s *ECS) Task(arn string) map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if task, ok := s.tasks[arn]; ok {
		return copyMap(task)
	}
	return nil
}

// Tasks returns copies of all task documents.
func (s *ECS) Tasks() []map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks := []map[string]interface{}{}
	for _, task := range s.tasks {
		tasks = append(tasks, copyMap(task))
	}
	return tasks
}

// Definition returns a copy of the task definition with the given ARN, or nil.
func (s *ECS) Definition(arn string) map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if definition, ok := s.definitions[arn]; ok {
		return copyMap(definition)
	}
	return nil
}

func (s *ECS) register(body map[string]interface{}) (int, interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	arn := fmt.Sprintf("arn:aws:ecs:%s:%s:task-definition/%s:%d", Region, Account, body["family"], s.n)
	definition := copyMap(body)
	definition["taskDefinitionArn"] = arn
	definition["revision"] = s.n
	definition["status"] = "ACTIVE"
	s.definitions[arn] = definition
	return 200, map[string]interface{}{"taskDefinition": definition}
}

func (s *ECS) describeTaskDefinition(body map[string]interface{}) (int, interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	definition, ok := s.definitions[fmt.Sprint(body["taskDefinition"])]
	if !ok {
		return 400, map[string]string{"__type": "ClientException", "message": "Unable to describe task definition."}
	}
	return 200, map[string]interface{}{"taskDefinition": definition}
}

func (s *ECS) deregister(body map[string]interface{}) (int, interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	definition, ok := s.definitions[fmt.Sprint(body["taskDefinition"])]
	if !ok {
		return 400, map[string]string{"__type": "ClientException", "message": "The specified task definition does not exist."}
	}
	definition["status"] = "INACTIVE"
	return 200, map[string]interface{}{"taskDefinition": definition}
}

func (s *ECS) run(body map[string]interface{}) (int, interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	definition, ok := s.definitions[fmt.Sprint(body["taskDefinition"])]
	if !ok {
		return 400, map[string]string{"__type": "ClientException", "message": "TaskDefinition not found."}
	}
	s.n++
	arn := fmt.Sprintf("arn:aws:ecs:%s:%s:task/%s/%032x", Region, Account, body["cluster"], s.n)
	containers := []interface{}{}
	for _, c := range definition["containerDefinitions"].([]interface{}) {
		containers = append(containers, map[string]interface{}{"name": c.(map[string]interface{})["name"], "lastStatus": "RUNNING"})
	}
	task := map[string]interface{}{
		"taskArn": arn, "clusterArn": ClusterArn(fmt.Sprint(body["cluster"])), "taskDefinitionArn": definition["taskDefinitionArn"],
		"lastStatus": "RUNNING", "desiredStatus": "RUNNING", "launchType": "FARGATE", "startedBy": body["startedBy"],
		"createdAt": 1.7e9, "cpu": definition["cpu"], "memory": definition["memory"],
		"attachments": []interface{}{map[string]interface{}{"type": "ElasticNetworkInterface", "status": "ATTACHED",
			"details": []interface{}{
				map[string]interface{}{"name": "subnetId", "value": Subnet},
				map[string]interface{}{"name": "privateIPv4Address", "value": TaskIPv4Address},
			}}},
		"containers": containers,
	}
	s.tasks[arn] = task
	return 200, map[string]interface{}{"tasks": []interface{}{task}, "failures": []interface{}{}}
}

func (s *ECS) describeTasks(body map[string]interface{}) (int, interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks, failures := []interface{}{}, []interface{}{}
	for _, arn := range body["tasks"].([]interface{}) {
		if task, ok := s.tasks[fmt.Sprint(arn)]; ok {
			tasks = append(tasks, task)
		} else {
			failures = append(failures, map[string]interface{}{"arn": arn, "reason": "MISSING"})
		}
	}
	return 200, map[string]interface{}{"tasks": tasks, "failures": failures}
}

func (s *ECS) list(body map[string]interface{}) (int, interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	wanted := "RUNNING"
	if body["desiredStatus"] != nil {
		wanted = fmt.Sprint(body["desiredStatus"])
	}
	arns := []interface{}{}
	for arn, task := range s.tasks {
		if task["desiredStatus"] == wanted {
			arns = append(arns, arn)
		}
	}
	return 200, map[string]interface{}{"taskArns": arns}
}

func (s *ECS) stop(body map[string]interface{}) (int, interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[fmt.Sprint(body["task"])]
	if !ok {
		return 400, map[string]string{"__type": "InvalidParameterException", "message": "The referenced task was not found."}
	}
	task["desiredStatus"] = "STOPPED"
	task["lastStatus"] = "STOPPED"
	task["stoppedReason"] = body["reason"]
	for _, c := range task["containers"].([]interface{}) {
		c.(map[string]interface{})["lastStatus"] = "STOPPED"
		c.(map[string]interface{})["exitCode"] = 0
	}
	return 200, map[string]interface{}{"task": task}
}

func copyMap(m map[string]interface{}) map[string]interface{} {
	raw, _ := json.Marshal(m)
	out := map[string]interface{}{}
	_ = json.Unmarshal(raw, &out)
	return out
}
