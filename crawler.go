package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"sync"
	"time"

	"github.com/gologme/log"
	"github.com/yggdrasil-network/yggdrasil-go/src/config"
	"github.com/yggdrasil-network/yggdrasil-go/src/crypto"
	"github.com/yggdrasil-network/yggdrasil-go/src/yggdrasil"
)

const retryCount = 5

type node struct {
	core              yggdrasil.Core
	config            *config.NodeConfig
	log               *log.Logger
	dhtWaitGroup      sync.WaitGroup
	dhtVisited        map[crypto.BoxPubKey]attempt
	dhtMutex          sync.RWMutex
	nodeInfoWaitGroup sync.WaitGroup
	nodeInfoVisited   struct {
		Meta struct {
			GeneratedAtUTC  int64
			NodesSuccessful int
			NodesFailed     int
		} `json:"meta"`
		NodeInfo map[string]interface{} `json:"nodeinfo"`
	}
	nodeInfoMutex sync.RWMutex
}

type attempt struct {
	coords []uint64 // the coordinates of the node
	found  bool     // has a search for this node completed successfully?
}

func main() {
	n := node{
		config: config.GenerateConfig(),
		log:    log.New(os.Stdout, "", log.Flags()),
	}

	n.dhtVisited = make(map[crypto.BoxPubKey]attempt)
	n.nodeInfoVisited.NodeInfo = make(map[string]interface{})

	n.config.NodeInfo = map[string]interface{}{
		"name": "Yggdrasil Crawler",
	}
	n.config.AdminListen = "unix:///var/run/yggcrawl.sock"
	n.config.SessionFirewall.Enable = true
	n.config.SessionFirewall.AllowFromDirect = false
	n.config.SessionFirewall.AllowFromRemote = false
	n.config.SessionFirewall.AlwaysAllowOutbound = false

	n.core.Start(n.config, n.log)
	n.core.AddPeer("tcp://edinburgh.psg.neilalexander.dev:54321", "")

	fmt.Println("Waiting for DHT bootstrap")
	for {
		if len(n.core.GetDHT()) > 3 {
			break
		}
		time.Sleep(time.Second)
	}
	fmt.Println("DHT bootstrap complete")

	starttime := time.Now()

	if key, err := hex.DecodeString(n.core.EncryptionPublicKey()); err == nil {
		var pubkey crypto.BoxPubKey
		copy(pubkey[:], key)
		n.dhtWaitGroup.Add(1)
		go n.dhtPing(pubkey, n.core.Coords())
	} else {
		panic("failed to decode pub key")
	}

	n.dhtWaitGroup.Wait()
	n.nodeInfoWaitGroup.Wait()

	n.dhtMutex.Lock()
	n.nodeInfoMutex.Lock()

	fmt.Println()
	fmt.Println("The crawl took", time.Since(starttime))

	attempted := len(n.dhtVisited)
	found := 0
	for _, attempt := range n.dhtVisited {
		if attempt.found {
			found++
		}
	}

	n.nodeInfoVisited.Meta.GeneratedAtUTC = time.Now().UTC().Unix()
	n.nodeInfoVisited.Meta.NodesSuccessful = len(n.nodeInfoVisited.NodeInfo)
	n.nodeInfoVisited.Meta.NodesFailed = found - len(n.nodeInfoVisited.NodeInfo)

	if j, err := json.Marshal(n.nodeInfoVisited); err == nil {
		if err := ioutil.WriteFile("nodeinfo.json", j, 0644); err != nil {
			fmt.Println("Failed to write nodeinfo.json:", err)
		}
	}

	fmt.Println(attempted, "nodes were processed")
	fmt.Println(found, "nodes were found")
	fmt.Println(attempted-found, "nodes were not found")
	fmt.Println()
	fmt.Println(n.nodeInfoVisited.Meta.NodesSuccessful, "nodes responded with nodeinfo")
	fmt.Println(n.nodeInfoVisited.Meta.NodesFailed, "nodes did not respond with nodeinfo")
}

func (n *node) dhtPing(pubkey crypto.BoxPubKey, coords []uint64) {
	defer n.dhtWaitGroup.Done()

	n.dhtMutex.RLock()
	if info := n.dhtVisited[pubkey]; info.found {
		n.dhtMutex.RUnlock()
		return
	}
	n.dhtMutex.RUnlock()

	var res yggdrasil.DHTRes
	var err error
	for idx := 0; idx < retryCount; idx++ {
		time.Sleep(time.Millisecond * time.Duration(rand.Intn(1000)*(1<<idx)))
		res, err = n.core.DHTPing(
			pubkey,
			coords,
			&crypto.NodeID{},
		)
		if err == nil {
			break
		}
	}

	info := attempt{
		coords: res.Coords,
		found:  err == nil,
	}

	n.dhtMutex.Lock()
	defer n.dhtMutex.Unlock()
	oldInfo := n.dhtVisited[pubkey]
	if info.found || !oldInfo.found {
		n.dhtVisited[pubkey] = info
	}

	if !oldInfo.found && info.found {
		n.nodeInfoWaitGroup.Add(1)
		go n.nodeInfo(pubkey, coords)
	} else {
		return
	}

	for _, info := range res.Infos {
		n.dhtWaitGroup.Add(1)
		go n.dhtPing(info.PublicKey, info.Coords)
	}
}

func (n *node) nodeInfo(pubkey crypto.BoxPubKey, coords []uint64) {
	defer n.nodeInfoWaitGroup.Done()

	nodeid := hex.EncodeToString(pubkey[:])

	n.nodeInfoMutex.RLock()
	if _, ok := n.nodeInfoVisited.NodeInfo[nodeid]; ok {
		n.nodeInfoMutex.RUnlock()
		return
	}
	n.nodeInfoMutex.RUnlock()

	var res yggdrasil.NodeInfoPayload
	var err error
	for idx := 0; idx < retryCount; idx++ {
		time.Sleep(time.Millisecond * time.Duration(rand.Intn(1000)*(1<<idx)))
		res, err = n.core.GetNodeInfo(pubkey, coords, false)
		if err == nil {
			break
		}
	}

	if err != nil {
		return
	}

	n.nodeInfoMutex.Lock()
	defer n.nodeInfoMutex.Unlock()

	var j interface{}
	if err := json.Unmarshal(res, &j); err != nil {
		fmt.Println(err)
	} else {
		n.nodeInfoVisited.NodeInfo[nodeid] = j
	}
}
