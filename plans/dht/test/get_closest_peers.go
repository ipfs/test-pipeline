package test

import (
	"context"
	"encoding/hex"
	"fmt"
	"github.com/libp2p/go-libp2p-core/peer"
	kbucket "github.com/libp2p/go-libp2p-kbucket"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/ipfs/go-cid"
	u "github.com/ipfs/go-ipfs-util"

	"github.com/ipfs/testground/sdk/runtime"
	"github.com/ipfs/testground/sdk/sync"
)

func GetClosestPeers(runenv *runtime.RunEnv) error {
	commonOpts := GetCommonOpts(runenv)
	recordCount := runenv.IntParam("record_count")
	finder := runenv.BooleanParam("search_records")

	ctx, cancel := context.WithTimeout(context.Background(), commonOpts.Timeout)
	defer cancel()

	watcher, writer := sync.MustWatcherWriter(ctx, runenv)
	//defer watcher.Close()
	//defer writer.Close()

	ri := &RunInfo{
		runenv:  runenv,
		watcher: watcher,
		writer:  writer,
	}

	node, others, err := Setup(ctx, ri, commonOpts)
	if err != nil {
		return err
	}

	defer Teardown(ctx, ri)

	stager := NewBatchStager(ctx, node.info.Seq, runenv.TestInstanceCount, "default", ri)

	t := time.Now()

	// Bring the network into a nice, stable, bootstrapped state.
	if err = Bootstrap(ctx, ri, commonOpts, node, others, stager, GetBootstrapNodes(commonOpts, node, others)); err != nil {
		return err
	}

	runenv.RecordMetric(&runtime.MetricDefinition{
		Name:           fmt.Sprintf("bs"),
		Unit:           "ns",
		ImprovementDir: -1,
	}, float64(time.Since(t).Nanoseconds()))

	if commonOpts.RandomWalk {
		if err = RandomWalk(ctx, runenv, node.dht); err != nil {
			return err
		}
	}

	t = time.Now()

	if err := SetupNetwork(ctx, ri, commonOpts.Latency); err != nil {
		return err
	}

	runenv.RecordMetric(&runtime.MetricDefinition{
		Name:           fmt.Sprintf("reset"),
		Unit:           "ns",
		ImprovementDir: -1,
	}, float64(time.Since(t).Nanoseconds()))

	t = time.Now()

	// Calculate the CIDs we're dealing with.
	cids := func() (out []cid.Cid) {
		for i := 0; i < recordCount; i++ {
			c := fmt.Sprintf("CID %d", i)
			out = append(out, cid.NewCidV0(u.Hash([]byte(c))))
		}
		return out
	}()

	stager.Reset("lookup")
	if err := stager.Begin(); err != nil {
		return err
	}

	runenv.RecordMessage("start gcp loop")
	runenv.RecordMessage(fmt.Sprintf("isFinder: %v, seqNo: %v, numFPeers %d, numRecords: %d", finder, node.info.Seq, recordCount, len(cids)))

	if finder {
		g := errgroup.Group{}
		for index, cid := range cids {
			i := index
			c := cid
			g.Go(func() error {
				p := peer.ID(c.Bytes())
				ectx, cancel := context.WithCancel(ctx)
				ectx = TraceQuery(ctx, runenv, node, p.Pretty())
				t := time.Now()
				pids, err := node.dht.GetClosestPeers(ectx, c.KeyString())
				cancel()

				peers := make([]peer.ID, 0, commonOpts.BucketSize)
				for p := range pids {
					peers = append(peers, p)
				}

				if err == nil {
					runenv.RecordMetric(&runtime.MetricDefinition{
						Name:           fmt.Sprintf("time-to-find-%d", i),
						Unit:           "ns",
						ImprovementDir: -1,
					}, float64(time.Since(t).Nanoseconds()))

					runenv.RecordMetric(&runtime.MetricDefinition{
						Name:           fmt.Sprintf("peers-found-%d", i),
						Unit:           "peers",
						ImprovementDir: 1,
					}, float64(len(pids)))

					actualClosest := getClosestPeerRanking(node, others, c)
					outputGCP(runenv, node.info.Addrs.ID, c, peers, actualClosest)
				} else {
					runenv.RecordMessage("Error during GCP %w", err)
				}
				return err
			})
		}

		if err := g.Wait(); err != nil {
			_ = stager.End()
			return fmt.Errorf("failed while finding providerss: %s", err)
		}
	}

	runenv.RecordMessage("done provide loop")

	if err := stager.End(); err != nil {
		return err
	}

	runenv.RecordMetric(&runtime.MetricDefinition{
		Name:           fmt.Sprintf("search"),
		Unit:           "ns",
		ImprovementDir: -1,
	}, float64(time.Since(t).Nanoseconds()))

	outputGraph(node.dht, "end")

	return nil
}

func getClosestPeerRanking(me *NodeParams, others map[peer.ID]*NodeInfo, target cid.Cid) []peer.ID {
	var allPeers []peer.ID
	allPeers = append(allPeers, me.dht.PeerID())
	for p := range others {
		allPeers = append(allPeers, p)
	}

	kadTarget := kbucket.ConvertKey(target.KeyString())
	return kbucket.SortClosestPeers(allPeers, kadTarget)
}

func outputGCP(runenv *runtime.RunEnv, me peer.ID, target cid.Cid, peers, rankedPeers []peer.ID) {
	peerStrs := make([]string, len(peers))
	kadPeerStrs := make([]string, len(peers))

	for i, p := range peers {
		peerStrs[i] = p.String()
		kadPeerStrs[i] = hex.EncodeToString(kbucket.ConvertKey(string(p)))
	}

	actualClosest := rankedPeers[:len(peers)]

	nodeLogger.Infow("gcp-results",
		"me", me.String(),
		"KadMe", kbucket.ConvertKey(string(me)),
		"target", target,
		"peers", peers,
		"actual", actualClosest,
		"KadTarget", kbucket.ConvertKey(target.KeyString()),
		"KadPeers", peerIDsToKadIDs(peers),
		"KadActual", peerIDsToKadIDs(actualClosest),
		"Scores", gcpScore(peers, rankedPeers),
	)

	nodeLogger.Sync()
}

func gcpScore(peers, rankedPeers []peer.ID) []int {
	getIndex := func(peers []peer.ID, target peer.ID) int {
		for i, p := range peers {
			if p == target {
				return i
			}
		}
		return -1
	}

	// score is distance between actual ranking and our ranking
	var scores []int
	for i, p := range peers {
		diff := getIndex(rankedPeers, p) - i
		scores = append(scores, diff)
	}
	return scores
}

func peerIDsToKadIDs(peers []peer.ID) []kbucket.ID {
	kadIDs := make([]kbucket.ID, len(peers))
	for i, p := range peers {
		kadIDs[i] = kbucket.ConvertPeerID(p)
	}
	return kadIDs
}
