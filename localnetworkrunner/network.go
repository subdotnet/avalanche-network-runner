package localnetworkrunner

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"syscall"
	"time"
    "errors"

    "github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanche-testing/avalanche/libs/avalanchegoclient"
	"github.com/ava-labs/avalanche-testing/logging"
	oldnetworkrunner "github.com/ava-labs/avalanche-testing/avalanche/builder/networkrunner"
    "github.com/ava-labs/avalanche-testing/avalanche/networkrunner"
	"github.com/ava-labs/avalanchego/config"
	ps "github.com/mitchellh/go-ps"
	"github.com/palantir/stacktrace"
)

type Network struct {
	procs map[ids.ID]*exec.Cmd
	nodes map[ids.ID]*Node
    nodeIDs map[ids.ID]string
}

type Node struct {
    Client *oldnetworkrunner.NodeRunner
}

func (node *Node) GetAPIClient() *oldnetworkrunner.NodeRunner {
    return Client
}

func createFile(fname string, contents []byte) error {
	if err := os.MkdirAll(path.Dir(fname), 0o750); err != nil {
		return err
	}
	file, err := os.Create(fname)
	if err != nil {
		return err
	}
    if _, err := file.Write(contents); err != nil {
        return err
    }
	file.Close()
	return nil
}

func NewNetwork(networkConfig NetworkConfig, binMap map[int]string) (*Network, error) {
	net := Network{}
	net.procs = map[string]*exec.Cmd{}
	net.nodes = map[string]*Node{}

	var configFlags map[string]interface{}
	if err := json.Unmarshal(networkConfig.CoreConfigFlags, &configFlags); err != nil {
		return nil, err
	}

    n := 0
	for _, nodeConfig := range networkConfig.NodeConfigs {
		if err := json.Unmarshal(nodeConfig.ConfigFlags, &configFlags); err != nil {
			return nil, err
		}

		configBytes, err := json.Marshal(configFlags)
		if err != nil {
			return nil, err
		}

		configDir := configFlags["chain-config-dir"].(string)
		configFilePath := path.Join(configDir, "config.json")
		cConfigFilePath := path.Join(configDir, "C", "config.json")

        if err := createFile(configFlags["genesis"].(string), networkConfig.Genesis); err != nil {
			return nil, err
        }
		if err := createFile(cConfigFilePath, networkConfig.CChainConfig); err != nil {
			return nil, err
        }
		if err := createFile(configFlags["staking-tls-cert-file"].(string), nodeConfig.Cert); err != nil {
			return nil, err
        }
		if err := createFile(configFlags["staking-tls-key-file"].(string), nodeConfig.PrivateKey); err != nil {
			return nil, err
        }
		if err := createFile(configFilePath, configBytes); err != nil {
			return nil, err
        }

		ch := make(chan string, 1)
		read, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		go func() {
			sc := bufio.NewScanner(read)
			for sc.Scan() {
				logging.Debugf("[%s] - %s\n", nodeConfig.NodeID, sc.Text())
			}
			close(ch)
		}()

		avalanchegoPath := binMap[nodeConfig.BinKind]
		configFileFlag := fmt.Sprintf("--%s=%s", config.ConfigFileKey, configFilePath)
		cmd := exec.Command(avalanchegoPath, configFileFlag)

		cmd.Stdout = w
		cmd.Stderr = w
		if err := cmd.Start(); err != nil {
			return nil, err
		}

        id := ids.ID{}
        id[0] = n
        n += 1

        nodeIDs[id] := nodeConfig.NodeID
		net.procs[id] = cmd

		nodeIP := configFlags["public-ip"].(string)
		nodePort := uint(configFlags["http-port"].(float64))

        nodeClient, _ := oldnetworkrunner.NewNodeRunnerFromFields(
            nodeConfig.NodeID,
            nodeConfig.NodeID,
            nodeIP,
            nodePort,
            avalanchegoclient.NewClient(nodeIP, nodePort, nodePort, 20*time.Second),
        )

		net.nodes[id] = &Node{nodeClient}
	}

	return &net, nil
}

func waitNode(client *avalanchegoclient.Client) bool {
	info := client.InfoAPI()
    timeout := 1 * time.Minute
    pollTime := 10 * time.Second
    nodeIsUp := false
    for t0 := time.Now(); !nodeIsUp && time.Since(t0) <= timeout; time.Sleep(pollTime) {
        nodeIsUp = true
	    if bootstrapped, err := info.IsBootstrapped("P"); err != nil || !bootstrapped {
           nodeIsUp = false
           continue
        }
	    if bootstrapped, err := info.IsBootstrapped("C"); err != nil || !bootstrapped {
           nodeIsUp = false
           continue
        }
	    if bootstrapped, err := info.IsBootstrapped("X"); err != nil || !bootstrapped {
           nodeIsUp = false
        }
    }
    return nodeIsUp
}

func (net *Network) Ready() (chan struct{}, chan error) {
    readyCh := make(chan struct{})
    errorCh := make(chan error)
    go func() {
        for k := range net.nodes {
            b := waitNode(net.nodes[k].Client.GetClient())
            if !b {
                errorCh <- errors.New(fmt.Sprintf("timeout waiting for %v", k))
            }
            logging.Infof("node %s is up\n", k)
        }
        readyCh <- struct{}{}
    }()
    return readyCh, errorCh
}

func (net *Network) GetNode(nodeID ids.ID) (networkrunner.Node, error) {
    node, ok := net.nodes[nodeID]
    if !ok {
        return nil, errors.New(fmt.Sprintf("node %s not found in network", nodeID))
    }
    return node, nil
}

func (net *Network) Stop() error {
	processes, err := ps.Processes()
	if err != nil {
		return stacktrace.Propagate(err, "unable to list processes")
	}
	for _, proc := range net.procs {
		procID := proc.Process.Pid
		if err := killProcessAndDescendants(procID, processes); err != nil {
			return err
		}
	}
	return nil
}

func killProcessAndDescendants(processID int, processes []ps.Process) error {
	// Kill descendants of [processID] in [processes]
	for _, process := range processes {
		if process.PPid() != processID {
			continue
		}
		if err := killProcessAndDescendants(process.Pid(), processes); err != nil {
			return stacktrace.Propagate(err, "unable to kill process and descendants")
		}
	}
	// Kill [processID]
	return syscall.Kill(processID, syscall.SIGTERM)
}