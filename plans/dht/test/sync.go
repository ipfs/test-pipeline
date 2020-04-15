package test

import (
	"github.com/ipfs/testground/sdk/sync"
)

// PeerAttribTopic represents a subtree under the test run's sync tree where peers
// participating in this distributed test advertise their attributes.
var PeerAttribTopic = sync.NewTopic("attribs", &DHTNodeInfo{})