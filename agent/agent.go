package agent

import (
	"fmt"
	"net"
	"time"

	docker "github.com/fsouza/go-dockerclient"

	"github.com/weaveworks/flux/common/daemon"
	"github.com/weaveworks/flux/common/netutil"
	"github.com/weaveworks/flux/common/store"
	"github.com/weaveworks/flux/common/store/etcdstore"
)

type DockerClient interface {
	Version() (*docker.Env, error)
	AddEventListener(listener chan<- *docker.APIEvents) error
	RemoveEventListener(listener chan *docker.APIEvents) error
	ListContainers(opts docker.ListContainersOptions) ([]docker.APIContainers, error)
	InspectContainer(id string) (*docker.Container, error)
}

type AgentConfig struct {
	// May be pre-set
	InstanceUpdates      chan<- InstanceUpdate
	InstanceUpdatesReset chan struct{}

	hostIP            net.IP
	network           string
	store             store.Store
	dockerClient      DockerClient
	reconnectInterval time.Duration
}

const (
	GLOBAL = "global"
	LOCAL  = "local"
)

func isValidNetworkMode(mode string) bool {
	return mode == GLOBAL || mode == LOCAL
}

func (cf *AgentConfig) Populate(deps *daemon.Dependencies) {
	deps.StringVar(&cf.network, "network-mode", LOCAL, fmt.Sprintf(`Kind of network to assume for containers (either "%s" or "%s")`, LOCAL, GLOBAL))
	deps.Dependency(etcdstore.StoreDependency(&cf.store))
	deps.Dependency(netutil.HostIPDependency(&cf.hostIP))
}

func (cf *AgentConfig) Prepare() (daemon.StartFunc, error) {
	if !isValidNetworkMode(cf.network) {
		return nil, fmt.Errorf("Unknown network mode '%s'", cf.network)
	}

	if cf.dockerClient == nil {
		var err error
		if cf.dockerClient, err = docker.NewClientFromEnv(); err != nil {
			return nil, err
		}
	}

	if cf.reconnectInterval == 0 {
		cf.reconnectInterval = 10 * time.Second
	}

	if cf.InstanceUpdatesReset == nil {
		cf.InstanceUpdatesReset = make(chan struct{}, 1)
	}

	containerUpdates := make(chan ContainerUpdate)
	containerUpdatesReset := make(chan struct{}, 1)
	serviceUpdates := make(chan store.ServiceUpdate)
	serviceUpdatesReset := make(chan struct{}, 1)

	instanceUpdates := make(chan InstanceUpdate)
	instanceUpdatesRead := instanceUpdates
	instanceUpdatesTee := daemon.NullStartFunc
	if cf.InstanceUpdates != nil {
		instanceUpdatesRead = make(chan InstanceUpdate)
		instanceUpdatesTee = Tee(instanceUpdates, cf.InstanceUpdates,
			instanceUpdatesRead)
	}

	syncInstConf := syncInstancesConfig{
		hostIP:  cf.hostIP,
		network: cf.network,

		containerUpdates:      containerUpdates,
		containerUpdatesReset: containerUpdatesReset,
		serviceUpdates:        serviceUpdates,
		serviceUpdatesReset:   serviceUpdatesReset,
		instanceUpdates:       instanceUpdates,
		instanceUpdatesReset:  cf.InstanceUpdatesReset,
	}

	setInstConf := setInstancesConfig{
		hostIP:               cf.hostIP,
		store:                cf.store,
		instanceUpdates:      instanceUpdatesRead,
		instanceUpdatesReset: cf.InstanceUpdatesReset,
	}

	return daemon.Aggregate(
		daemon.Reset(containerUpdatesReset,
			daemon.Restart(cf.reconnectInterval,
				dockerListenerStartFunc(cf.dockerClient,
					containerUpdates))),

		daemon.Reset(serviceUpdatesReset,
			daemon.Restart(cf.reconnectInterval,
				store.WatchServicesStartFunc(cf.store,
					store.QueryServiceOptions{WithContainerRules: true},
					serviceUpdates))),

		daemon.Restart(cf.reconnectInterval, syncInstConf.StartFunc()),
		daemon.Restart(cf.reconnectInterval, setInstConf.StartFunc()),
		instanceUpdatesTee), nil
}
