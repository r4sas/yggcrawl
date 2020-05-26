// yggcrawl
// Copyright (C) 2020 Neil Alexander
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// 	the Free Software Foundation, either version 3 of the License, or
// 	(at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net"
	"os"
	"sync"
	"time"

	"github.com/gologme/log"
	"github.com/yggdrasil-network/yggdrasil-go/src/address"
	"github.com/yggdrasil-network/yggdrasil-go/src/config"
	"github.com/yggdrasil-network/yggdrasil-go/src/crypto"
	"github.com/yggdrasil-network/yggdrasil-go/src/multicast"
	"github.com/yggdrasil-network/yggdrasil-go/src/yggdrasil"
)

var defaultPeer = flag.String("peer", "", "static peer to use, e.g. tcp://host:port")
var defaultMulticast = flag.Bool("multicast", false, "whether to enable multicast peering")
var defaultFilename = flag.String("file", "results.json", "filename to write results to")
var defaultAdminSocket = flag.String("admin", "none", "admin socket path, e.g. unix:///var/run/yggcrawl.sock")
var defaultRetryCount = flag.Int("retry", 5, "number of retry attempts (with random exponential backoff starting at 1s)")

type node struct {
	core              yggdrasil.Core
	multicast         multicast.Multicast
	state             config.NodeState
	config            *config.NodeConfig
	log               *log.Logger
	dhtWaitGroup      sync.WaitGroup
	dhtVisited        map[string]attempt
	dhtMutex          sync.RWMutex
	nodeInfoWaitGroup sync.WaitGroup
	nodeInfoVisited   map[string]interface{}
	nodeInfoMutex     sync.RWMutex
}

// This is the structure that we marshal at the end into JSON results
type results struct {
	Meta struct {
		GeneratedAtUTC     int64   `json:"generated_at_utc"`
		TimeTaken          float64 `json:"crawl_time_seconds"`
		NodesAttempted     int     `json:"nodes_attempted"`
		NodesSuccessful    int     `json:"nodes_successful"`
		NodesFailed        int     `json:"nodes_failed"`
		NodeInfoSuccessful int     `json:"nodeinfo_successful"`
		NodeInfoFailed     int     `json:"nodeinfo_failed"`
	} `json:"meta"`
	Topology *map[string]attempt     `json:"topology"`
	NodeInfo *map[string]interface{} `json:"nodeinfo"`
}

type attempt struct {
	NodeID     string   `json:"node_id"`     // the node ID
	IPv6Addr   string   `json:"ipv6_addr"`   // the node address
	IPv6Subnet string   `json:"ipv6_subnet"` // the node subnet
	Coords     []uint64 `json:"coords"`      // the coordinates of the node
	Found      bool     `json:"found"`       // has a search for this node completed successfully?
}

func main() {
	flag.Parse()

	n := node{
		config: config.GenerateConfig(),
		log:    log.New(os.Stdout, "", log.Flags()),
	}

	if *defaultPeer == "" && *defaultMulticast == false {
		fmt.Println("No peer has been specified, see -help")
		return
	}

	n.dhtVisited = make(map[string]attempt)
	n.nodeInfoVisited = make(map[string]interface{})

	n.config.NodeInfo = map[string]interface{}{
		"name": "Yggdrasil Crawler",
	}
	n.config.AdminListen = *defaultAdminSocket
	n.config.SessionFirewall.Enable = true
	n.config.SessionFirewall.AllowFromDirect = false
	n.config.SessionFirewall.AllowFromRemote = false
	n.config.SessionFirewall.AlwaysAllowOutbound = false
	n.core.Start(n.config, n.log)

	if *defaultPeer != "" {
		if err := n.core.CallPeer(*defaultPeer, ""); err != nil {
			fmt.Println("Failed to connect to peer:", err)
			return
		}
	}

	if *defaultMulticast {
		if err := n.multicast.Init(&n.core, &n.state, n.log, nil); err != nil {
			log.Errorln("An error occurred initialising multicast:", err)
			return
		}
		if err := n.multicast.Start(); err != nil {
			log.Errorln("An error occurred starting multicast:", err)
			return
		}
		fmt.Println("Multicast is enabled on", len(n.multicast.Interfaces()), "interface(s)")
	}

	fmt.Println("Waiting for peers")
	for {
		if len(n.core.GetSwitchPeers()) > 0 {
			break
		}
		time.Sleep(time.Second)
	}
	fmt.Println("Connected to", len(n.core.GetSwitchPeers()), "peer(s)")

	fmt.Println("Waiting for DHT bootstrap")
	for {
		if len(n.core.GetDHT()) > 3 {
			break
		}
		time.Sleep(time.Second)
	}
	fmt.Println("DHT bootstrap complete")

	starttime := time.Now()
	fmt.Println("Our network coords are", n.core.Coords())
	fmt.Println("Starting crawl")

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
		if attempt.Found {
			found++
		}
	}

	res := results{
		Topology: &n.dhtVisited,
		NodeInfo: &n.nodeInfoVisited,
	}
	res.Meta.GeneratedAtUTC = time.Now().UTC().Unix()
	res.Meta.TimeTaken = time.Since(starttime).Seconds()
	res.Meta.NodeInfoSuccessful = len(n.nodeInfoVisited)
	res.Meta.NodeInfoFailed = found - len(n.nodeInfoVisited)
	res.Meta.NodesAttempted = attempted
	res.Meta.NodesSuccessful = found
	res.Meta.NodesFailed = attempted - found

	if j, err := json.MarshalIndent(res, "", "\t"); err == nil {
		if err := ioutil.WriteFile(*defaultFilename, j, 0644); err != nil {
			fmt.Printf("Failed to write %s: %v", *defaultFilename, err)
		} else {
			fmt.Println("Results written to", *defaultFilename)
		}
	} else {
		fmt.Println("Failed to marshal results:", err)
	}

	fmt.Println()
	fmt.Println(res.Meta.NodesAttempted, "nodes were processed")
	fmt.Println(res.Meta.NodesSuccessful, "nodes were found")
	fmt.Println(res.Meta.NodesFailed, "nodes were not found")
	fmt.Println()
	fmt.Println(res.Meta.NodesSuccessful, "nodes responded with nodeinfo")
	fmt.Println(res.Meta.NodesFailed, "nodes did not respond with nodeinfo")
}

func (n *node) dhtPing(pubkey crypto.BoxPubKey, coords []uint64) {
	// Notify the main goroutine that we're done working
	defer n.dhtWaitGroup.Done()

	// Generate useful information about the node, such as it's node ID, address
	// and subnet
	key := hex.EncodeToString(pubkey[:])
	nodeid := crypto.GetNodeID(&pubkey)
	addr := net.IP(address.AddrForNodeID(nodeid)[:])
	upper := append(address.SubnetForNodeID(nodeid)[:], 0, 0, 0, 0, 0, 0, 0, 0)
	subnet := net.IPNet{IP: upper, Mask: net.CIDRMask(64, 128)}

	// If we already have an entry of this node then we should stop what we're
	// doing - it either means that a search is already taking place, or that we
	// have already processed this node
	n.dhtMutex.RLock()
	if info := n.dhtVisited[key]; info.Found {
		n.dhtMutex.RUnlock()
		return
	}
	n.dhtMutex.RUnlock()

	// Make a record of this node and the coordinates so that future goroutines
	// started by a rumour of this node will not repeat this search
	var res yggdrasil.DHTRes
	var err error
	for idx := 0; idx < *defaultRetryCount; idx++ {
		// Randomized delay between attempts, increases exponentially
		time.Sleep(time.Millisecond * time.Duration(rand.Intn(1000)*(1<<idx)))
		// Send out a DHT ping request into the network
		res, err = n.core.DHTPing(
			pubkey,
			coords,
			&crypto.NodeID{},
		)
		if err == nil {
			break
		}
	}

	// Write our new information into the list of visited DHT nodes
	info := attempt{
		NodeID:     nodeid.String(),
		IPv6Addr:   addr.String(),
		IPv6Subnet: subnet.String(),
		Coords:     coords,
		Found:      err == nil,
	}

	// If we successfully found the node then update dhtVisited to say so
	n.dhtMutex.Lock()
	defer n.dhtMutex.Unlock()
	oldInfo := n.dhtVisited[key]
	if info.Found || !oldInfo.Found {
		n.dhtVisited[key] = info
	}

	// If this was the first response from this node then request nodeinfo
	if !oldInfo.Found && info.Found {
		n.nodeInfoWaitGroup.Add(1)
		go n.nodeInfo(pubkey, coords)
	} else {
		// This isn't our first response from the node, so don't do anything
		return
	}

	// Start new DHT search goroutines based on the rumours included in the DHT
	// ping response
	for _, info := range res.Infos {
		n.dhtWaitGroup.Add(1)
		go n.dhtPing(info.PublicKey, info.Coords)
	}
}

func (n *node) nodeInfo(pubkey crypto.BoxPubKey, coords []uint64) {
	// Notify the main goroutine that we're done working
	defer n.nodeInfoWaitGroup.Done()

	// Store information that says that we attempted to query this node for
	// nodeinfo
	key := hex.EncodeToString(pubkey[:])
	n.nodeInfoMutex.RLock()
	if _, ok := n.nodeInfoVisited[key]; ok {
		n.nodeInfoMutex.RUnlock()
		return
	}
	n.nodeInfoMutex.RUnlock()

	// send the nodeinfo request to the given coordinates
	var res yggdrasil.NodeInfoPayload
	var err error
	for idx := 0; idx < *defaultRetryCount; idx++ {
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

	// If we received nodeinfo back then try to unmarshal it and store it in our
	// received nodeinfos
	var j interface{}
	if err := json.Unmarshal(res, &j); err != nil {
		fmt.Println(err)
	} else {
		n.nodeInfoVisited[key] = j
	}
}
