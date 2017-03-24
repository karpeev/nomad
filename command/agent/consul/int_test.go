package consul_test

import (
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	consulapi "github.com/hashicorp/consul/api"
	"github.com/hashicorp/consul/testutil"
	"github.com/hashicorp/nomad/client"
	"github.com/hashicorp/nomad/client/allocdir"
	"github.com/hashicorp/nomad/client/config"
	"github.com/hashicorp/nomad/client/driver"
	"github.com/hashicorp/nomad/client/vaultclient"
	"github.com/hashicorp/nomad/command/agent/consul"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
)

func testLogger() *log.Logger {
	if testing.Verbose() {
		return log.New(os.Stderr, "", log.LstdFlags)
	}
	return log.New(ioutil.Discard, "", 0)
}

// TestConsul_Integration asserts TaskRunner properly registers and deregisters
// services and checks with Consul using an embedded Consul agent.
func TestConsul_Integration(t *testing.T) {
	if _, ok := driver.BuiltinDrivers["mock_driver"]; !ok {
		t.Skip(`test requires mock_driver; run with "-tags nomad_test"`)
	}
	if testing.Short() {
		t.Skip("-short set; skipping")
	}
	if unix.Geteuid() != 0 {
		t.Skip("Must be run as root")
	}
	// Create an embedded Consul server
	testconsul := testutil.NewTestServerConfig(t, func(c *testutil.TestServerConfig) {
		// If -v wasn't specified squelch consul logging
		if !testing.Verbose() {
			c.Stdout = ioutil.Discard
			c.Stderr = ioutil.Discard
		}
	})
	defer testconsul.Stop()

	conf := config.DefaultConfig()
	conf.ConsulConfig.Addr = testconsul.HTTPAddr
	consulConfig, err := conf.ConsulConfig.ApiConfig()
	if err != nil {
		t.Fatalf("error generating consul config: %v", err)
	}

	conf.StateDir = os.TempDir()
	defer os.RemoveAll(conf.StateDir)
	conf.AllocDir = os.TempDir()
	defer os.RemoveAll(conf.AllocDir)

	alloc := mock.Alloc()
	task := alloc.Job.TaskGroups[0].Tasks[0]
	task.Driver = "mock_driver"
	task.Config = map[string]interface{}{
		"run_for": "1h",
	}
	// Choose a port that shouldn't be in use
	task.Resources.Networks[0].ReservedPorts = []structs.Port{{Label: "http", Value: 3}}
	task.Services = []*structs.Service{
		{
			Name:      "httpd",
			PortLabel: "http",
			Tags:      []string{"nomad", "test", "http"},
			Checks: []*structs.ServiceCheck{
				{
					Name:      "httpd-http-check",
					Type:      "http",
					Path:      "/",
					Protocol:  "http",
					PortLabel: "http",
					Interval:  9000 * time.Hour,
					Timeout:   10 * time.Second,
				},
				{
					Name:     "httpd-script-check",
					Type:     "script",
					Command:  "/bin/true",
					Interval: 10 * time.Second,
					Timeout:  10 * time.Second,
				},
			},
		},
		{
			Name:      "httpd2",
			PortLabel: "http",
			Tags:      []string{"test", "http2"},
		},
	}

	logger := testLogger()
	logUpdate := func(name, state string, event *structs.TaskEvent) {
		debug.PrintStack()
		logger.Printf("[TEST] updater: name=%q state=%q event=%v", name, state, event)
	}
	allocDir := allocdir.NewAllocDir(logger, filepath.Join(conf.AllocDir, alloc.ID))
	if err := allocDir.Build(); err != nil {
		t.Fatalf("error building alloc dir: %v", err)
	}
	taskDir := allocDir.NewTaskDir(task.Name)
	vclient := vaultclient.NewMockVaultClient()
	consulClient, err := consulapi.NewClient(consulConfig)
	if err != nil {
		t.Fatalf("error creating consul client: %v", err)
	}
	serviceClient := consul.NewServiceClient(consulClient.Agent(), logger)
	defer serviceClient.Shutdown() // just-in-case cleanup
	consulRan := make(chan struct{})
	go func() {
		serviceClient.Run()
		close(consulRan)
	}()
	tr := client.NewTaskRunner(logger, conf, logUpdate, taskDir, alloc, task, vclient, serviceClient)
	tr.MarkReceived()
	ran := make(chan struct{})
	go func() {
		tr.Run()
		close(ran)
	}()

	// Block waiting for the service to appear
	catalog := consulClient.Catalog()
	res, meta, err := catalog.Service("httpd2", "test", nil)
	if len(res) == 0 {
		// Expected initial request to fail, do a blocking query
		res, meta, err = catalog.Service("httpd2", "test", &consulapi.QueryOptions{WaitIndex: meta.LastIndex + 1, WaitTime: 10 * time.Second})
		if err != nil {
			t.Fatalf("error querying for service: %v", err)
		}
	}
	if len(res) != 1 {
		t.Fatalf("expected 1 service but found %d:\n%#v", len(res), res)
	}

	// Assert the service with the checks exists
	res, meta, err = catalog.Service("httpd", "http", &consulapi.QueryOptions{WaitIndex: meta.LastIndex + 1, WaitTime: 10 * time.Second})
	if err != nil {
		t.Fatalf("error querying for service: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("exepcted 1 service but found %d:\n%#v", len(res), res)
	}

	// Assert the script check passes (mock_driver script checks always pass)
	checks, meta, err := consulClient.Health().Check("httpd", &consulapi.QueryOptions{WaitIndex: meta.LastIndex + 1, WaitTime: 10 * time.Second})
	panic("TODO")

	// Kill the task
	tr.Kill("", "", false)

	select {
	case <-ran:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for Run() to exit")
	}

	// Ensure Consul is clean
	//TODO
}
