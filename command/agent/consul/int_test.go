package consul_test

import (
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

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
					Interval: time.Second,
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
	noopUpdate := func(name, state string, event *structs.TaskEvent) {}
	allocDir := allocdir.NewAllocDir(logger, filepath.Join(conf.AllocDir, alloc.ID))
	taskDir := allocDir.NewTaskDir(task.Name)
	vclient := vaultclient.NewMockVaultClient()
	consulClient, err := consulapi.NewClient(consulConfig)
	if err != nil {
		t.Fatalf("error creating consul client: %v", err)
	}
	serviceClient := consul.NewServiceClient(consulClient.Agent(), logger)
	tr := client.NewTaskRunner(logger, conf, noopUpdate, taskDir, alloc, task, vclient, serviceClient)
	tr.MarkReceived()
	ran := make(chan struct{})
	go func() {
		tr.Run()
		close(ran)
	}()

	//TODO Actually test something!
	tr.Kill("", "", false)
	<-ran
}
