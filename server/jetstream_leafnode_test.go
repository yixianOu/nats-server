// Copyright 2020-2025 The NATS Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build !skip_js_tests

package server

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	jwt "github.com/nats-io/jwt/v2"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
)

func TestJetStreamLeafNodeUniqueServerNameCrossJSDomain(t *testing.T) {
	name := "NOT-UNIQUE"
	test := func(t *testing.T, s *Server, sIdExpected string, srvs ...*Server) {
		ids := map[string]string{}
		for _, srv := range srvs {
			checkLeafNodeConnectedCount(t, srv, 2)
			ids[srv.ID()] = srv.opts.JetStreamDomain
		}
		// ensure that an update for every server was received
		sysNc := natsConnect(t, fmt.Sprintf("nats://admin:s3cr3t!@127.0.0.1:%d", s.opts.Port))
		defer sysNc.Close()
		sub, err := sysNc.SubscribeSync(fmt.Sprintf(serverStatsSubj, "*"))
		require_NoError(t, err)
		for {
			m, err := sub.NextMsg(time.Second)
			require_NoError(t, err)
			tk := strings.Split(m.Subject, ".")
			if domain, ok := ids[tk[2]]; ok {
				delete(ids, tk[2])
				require_Contains(t, string(m.Data), fmt.Sprintf(`"domain":"%s"`, domain))
			}
			if len(ids) == 0 {
				break
			}
		}
		cnt := 0
		s.nodeToInfo.Range(func(key, value any) bool {
			cnt++
			require_Equal(t, value.(nodeInfo).name, name)
			require_Equal(t, value.(nodeInfo).id, sIdExpected)
			return true
		})
		require_Equal(t, cnt, 1)
	}
	tmplA := `
		listen: -1
		server_name: %s
		jetstream {
			max_mem_store: 256MB,
			max_file_store: 2GB,
			store_dir: '%s',
			domain: hub
		}
		accounts {
			JSY { users = [ { user: "y", pass: "p" } ]; jetstream: true }
			$SYS { users = [ { user: "admin", pass: "s3cr3t!" } ] }
		}
		leaf {
			port: -1
		}
    `
	tmplL := `
		listen: -1
		server_name: %s
		jetstream {
			max_mem_store: 256MB,
			max_file_store: 2GB,
			store_dir: '%s',
			domain: %s
		}
		accounts {
			JSY { users = [ { user: "y", pass: "p" } ]; jetstream: true }
			$SYS { users = [ { user: "admin", pass: "s3cr3t!" } ] }
		}
		leaf {
			remotes [
				{ urls: [ %s ], account: "JSY" }
				{ urls: [ %s ], account: "$SYS" }
			]
		}
    `
	t.Run("same-domain", func(t *testing.T) {
		confA := createConfFile(t, []byte(fmt.Sprintf(tmplA, name, t.TempDir())))
		sA, oA := RunServerWithConfig(confA)
		defer sA.Shutdown()
		// using same domain as sA
		confL := createConfFile(t, []byte(fmt.Sprintf(tmplL, name, t.TempDir(), "hub",
			fmt.Sprintf("nats://y:p@127.0.0.1:%d", oA.LeafNode.Port),
			fmt.Sprintf("nats://admin:s3cr3t!@127.0.0.1:%d", oA.LeafNode.Port))))
		sL, _ := RunServerWithConfig(confL)
		defer sL.Shutdown()
		// as server name uniqueness is violates, sL.ID() is the expected value
		test(t, sA, sL.ID(), sA, sL)
	})
	t.Run("different-domain", func(t *testing.T) {
		confA := createConfFile(t, []byte(fmt.Sprintf(tmplA, name, t.TempDir())))
		sA, oA := RunServerWithConfig(confA)
		defer sA.Shutdown()
		// using different domain as sA
		confL := createConfFile(t, []byte(fmt.Sprintf(tmplL, name, t.TempDir(), "spoke",
			fmt.Sprintf("nats://y:p@127.0.0.1:%d", oA.LeafNode.Port),
			fmt.Sprintf("nats://admin:s3cr3t!@127.0.0.1:%d", oA.LeafNode.Port))))
		sL, _ := RunServerWithConfig(confL)
		defer sL.Shutdown()
		checkLeafNodeConnectedCount(t, sL, 2)
		checkLeafNodeConnectedCount(t, sA, 2)
		// ensure sA contains only sA.ID
		test(t, sA, sA.ID(), sA, sL)
	})
}

func TestJetStreamLeafNodeJwtPermsAndJSDomains(t *testing.T) {
	createAcc := func(js bool) (string, string, nkeys.KeyPair) {
		kp, _ := nkeys.CreateAccount()
		aPub, _ := kp.PublicKey()
		claim := jwt.NewAccountClaims(aPub)
		if js {
			claim.Limits.JetStreamLimits = jwt.JetStreamLimits{
				MemoryStorage: 1024 * 1024,
				DiskStorage:   1024 * 1024,
				Streams:       1, Consumer: 2}
		}
		aJwt, err := claim.Encode(oKp)
		require_NoError(t, err)
		return aPub, aJwt, kp
	}
	sysPub, sysJwt, sysKp := createAcc(false)
	accPub, accJwt, accKp := createAcc(true)
	noExpiration := time.Now().Add(time.Hour)
	// create user for acc to be used in leaf node.
	lnCreds := createUserWithLimit(t, accKp, noExpiration, func(j *jwt.UserPermissionLimits) {
		j.Sub.Deny.Add("subdeny")
		j.Pub.Deny.Add("pubdeny")
	})
	unlimitedCreds := createUserWithLimit(t, accKp, noExpiration, nil)

	sysCreds := createUserWithLimit(t, sysKp, noExpiration, nil)

	tmplA := `
operator: %s
system_account: %s
resolver: MEMORY
resolver_preload: {
  %s: %s
  %s: %s
}
listen: 127.0.0.1:-1
leafnodes: {
	listen: 127.0.0.1:-1
}
jetstream :{
    domain: "cluster"
    store_dir: '%s'
    max_mem: 100Mb
    max_file: 100Mb
}
`

	tmplL := `
listen: 127.0.0.1:-1
accounts :{
    A:{   jetstream: enable, users:[ {user:a1,password:a1}]},
    SYS:{ users:[ {user:s1,password:s1}]},
}
system_account = SYS
jetstream: {
    domain: ln1
    store_dir: '%s'
    max_mem: 50Mb
    max_file: 50Mb
}
leafnodes:{
    remotes:[{ url:nats://127.0.0.1:%d, account: A, credentials: '%s'},
			 { url:nats://127.0.0.1:%d, account: SYS, credentials: '%s'}]
}
`

	confA := createConfFile(t, []byte(fmt.Sprintf(tmplA, ojwt, sysPub,
		sysPub, sysJwt, accPub, accJwt,
		t.TempDir())))
	sA, _ := RunServerWithConfig(confA)
	defer sA.Shutdown()

	confL := createConfFile(t, []byte(fmt.Sprintf(tmplL, t.TempDir(),
		sA.opts.LeafNode.Port, lnCreds, sA.opts.LeafNode.Port, sysCreds)))
	sL, _ := RunServerWithConfig(confL)
	defer sL.Shutdown()

	checkLeafNodeConnectedCount(t, sA, 2)
	checkLeafNodeConnectedCount(t, sL, 2)

	ncA := natsConnect(t, sA.ClientURL(), nats.UserCredentials(unlimitedCreds))
	defer ncA.Close()

	ncL := natsConnect(t, fmt.Sprintf("nats://a1:a1@127.0.0.1:%d", sL.opts.Port))
	defer ncL.Close()

	test := func(subject string, cSub, cPub *nats.Conn, remoteServerForSub *Server, accName string, pass bool) {
		t.Helper()
		sub, err := cSub.SubscribeSync(subject)
		require_NoError(t, err)
		require_NoError(t, cSub.Flush())
		// ensure the subscription made it across, or if not sent due to sub deny, make sure it could have made it.
		if remoteServerForSub == nil {
			time.Sleep(200 * time.Millisecond)
		} else {
			checkSubInterest(t, remoteServerForSub, accName, subject, time.Second)
		}
		require_NoError(t, cPub.Publish(subject, []byte("hello world")))
		require_NoError(t, cPub.Flush())
		m, err := sub.NextMsg(500 * time.Millisecond)
		if pass {
			require_NoError(t, err)
			require_True(t, m.Subject == subject)
			require_Equal(t, string(m.Data), "hello world")
		} else {
			require_True(t, err == nats.ErrTimeout)
		}
	}

	t.Run("sub-on-ln-pass", func(t *testing.T) {
		test("sub", ncL, ncA, sA, accPub, true)
	})
	t.Run("sub-on-ln-fail", func(t *testing.T) {
		test("subdeny", ncL, ncA, nil, "", false)
	})
	t.Run("pub-on-ln-pass", func(t *testing.T) {
		test("pub", ncA, ncL, sL, "A", true)
	})
	t.Run("pub-on-ln-fail", func(t *testing.T) {
		test("pubdeny", ncA, ncL, nil, "A", false)
	})
}

func TestJetStreamLeafNodeClusterExtensionWithSystemAccount(t *testing.T) {
	/*
		Topologies tested here
		same == true
		A  <-> B
		^ |\
		|   \
		|  proxy
		|     \
		LA <-> LB

		same == false
		A  <-> B
		^      ^
		|      |
		|    proxy
		|      |
		LA <-> LB

		The proxy is turned on later, such that the system account connection can be started later, in a controlled way
		This explicitly tests the system state before and after this happens.
	*/

	tmplA := `
listen: 127.0.0.1:-1
accounts :{
    A:{   jetstream: enable, users:[ {user:a1,password:a1}]},
    SYS:{ users:[ {user:s1,password:s1}]},
}
system_account: SYS
leafnodes: {
	listen: 127.0.0.1:-1
	no_advertise: true
	authorization: {
		timeout: 0.5
	}
}
jetstream :{
    domain: "cluster"
    store_dir: '%s'
    max_mem: 100Mb
    max_file: 100Mb
}
server_name: A
cluster: {
	name: clust1
	listen: 127.0.0.1:20104
	routes=[nats-route://127.0.0.1:20105]
	no_advertise: true
}
`

	tmplB := `
listen: 127.0.0.1:-1
accounts :{
    A:{   jetstream: enable, users:[ {user:a1,password:a1}]},
    SYS:{ users:[ {user:s1,password:s1}]},
}
system_account: SYS
leafnodes: {
	listen: 127.0.0.1:-1
	no_advertise: true
	authorization: {
		timeout: 0.5
	}
}
jetstream: {
    domain: "cluster"
    store_dir: '%s'
    max_mem: 100Mb
    max_file: 100Mb
}
server_name: B
cluster: {
	name: clust1
	listen: 127.0.0.1:20105
	routes=[nats-route://127.0.0.1:20104]
	no_advertise: true
}
`

	tmplLA := `
listen: 127.0.0.1:-1
accounts :{
    A:{   jetstream: enable, users:[ {user:a1,password:a1}]},
    SYS:{ users:[ {user:s1,password:s1}]},
}
system_account = SYS
jetstream: {
    domain: "cluster"
    store_dir: '%s'
    max_mem: 50Mb
    max_file: 50Mb
	%s
}
server_name: LA
cluster: {
	name: clustL
	listen: 127.0.0.1:20106
	routes=[nats-route://127.0.0.1:20107]
	no_advertise: true
}
leafnodes:{
	no_advertise: true
    remotes:[{url:nats://a1:a1@127.0.0.1:%d, account: A},
		     {url:nats://s1:s1@127.0.0.1:%d, account: SYS}]
}
`

	tmplLB := `
listen: 127.0.0.1:-1
accounts :{
    A:{   jetstream: enable, users:[ {user:a1,password:a1}]},
    SYS:{ users:[ {user:s1,password:s1}]},
}
system_account = SYS
jetstream: {
    domain: "cluster"
    store_dir: '%s'
    max_mem: 50Mb
    max_file: 50Mb
	%s
}
server_name: LB
cluster: {
	name: clustL
	listen: 127.0.0.1:20107
	routes=[nats-route://127.0.0.1:20106]
	no_advertise: true
}
leafnodes:{
	no_advertise: true
    remotes:[{url:nats://a1:a1@127.0.0.1:%d, account: A},
		     {url:nats://s1:s1@127.0.0.1:%d, account: SYS}]
}
`

	for _, testCase := range []struct {
		// which topology to pick
		same bool
		// If leaf server should be operational and form a Js cluster prior to joining.
		// In this setup this would be an error as you give the wrong hint.
		// But this should work itself out regardless
		leafFunctionPreJoin bool
	}{
		{true, true},
		{true, false},
		{false, true},
		{false, false}} {
		t.Run(fmt.Sprintf("%t-%t", testCase.same, testCase.leafFunctionPreJoin), func(t *testing.T) {
			sd1 := t.TempDir()
			confA := createConfFile(t, []byte(fmt.Sprintf(tmplA, sd1)))
			sA, _ := RunServerWithConfig(confA)
			defer sA.Shutdown()

			sd2 := t.TempDir()
			confB := createConfFile(t, []byte(fmt.Sprintf(tmplB, sd2)))
			sB, _ := RunServerWithConfig(confB)
			defer sB.Shutdown()

			checkClusterFormed(t, sA, sB)

			c := cluster{t: t, servers: []*Server{sA, sB}}
			c.waitOnLeader()

			// starting this will allow the second remote in tmplL to successfully connect.
			port := sB.opts.LeafNode.Port
			if testCase.same {
				port = sA.opts.LeafNode.Port
			}
			p := &proxyAcceptDetectFailureLate{acceptPort: port}
			defer p.close()
			lPort := p.runEx(t, true)

			hint := ""
			if testCase.leafFunctionPreJoin {
				hint = fmt.Sprintf("extension_hint: %s", strings.ToUpper(jsNoExtend))
			}

			sd3 := t.TempDir()
			// deliberately pick server sA and proxy
			confLA := createConfFile(t, []byte(fmt.Sprintf(tmplLA, sd3, hint, sA.opts.LeafNode.Port, lPort)))
			sLA, _ := RunServerWithConfig(confLA)
			defer sLA.Shutdown()

			sd4 := t.TempDir()
			// deliberately pick server sA and proxy
			confLB := createConfFile(t, []byte(fmt.Sprintf(tmplLB, sd4, hint, sA.opts.LeafNode.Port, lPort)))
			sLB, _ := RunServerWithConfig(confLB)
			defer sLB.Shutdown()

			checkClusterFormed(t, sLA, sLB)

			strmCfg := func(name, placementCluster string) *nats.StreamConfig {
				if placementCluster == "" {
					return &nats.StreamConfig{Name: name, Replicas: 1, Subjects: []string{name}}
				}
				return &nats.StreamConfig{Name: name, Replicas: 1, Subjects: []string{name},
					Placement: &nats.Placement{Cluster: placementCluster}}
			}
			// Only after the system account is fully connected can streams be placed anywhere.
			testJSFunctions := func(pass bool) {
				ncA := natsConnect(t, fmt.Sprintf("nats://a1:a1@127.0.0.1:%d", sA.opts.Port))
				defer ncA.Close()
				jsA, err := ncA.JetStream()
				require_NoError(t, err)
				_, err = jsA.AddStream(strmCfg(fmt.Sprintf("fooA1-%t", pass), ""))
				require_NoError(t, err)
				_, err = jsA.AddStream(strmCfg(fmt.Sprintf("fooA2-%t", pass), "clust1"))
				require_NoError(t, err)
				_, err = jsA.AddStream(strmCfg(fmt.Sprintf("fooA3-%t", pass), "clustL"))
				if pass {
					require_NoError(t, err)
				} else {
					require_Error(t, err)
					require_Contains(t, err.Error(), "no suitable peers for placement")
				}
				ncL := natsConnect(t, fmt.Sprintf("nats://a1:a1@127.0.0.1:%d", sLA.opts.Port))
				defer ncL.Close()
				jsL, err := ncL.JetStream()
				require_NoError(t, err)
				_, err = jsL.AddStream(strmCfg(fmt.Sprintf("fooL1-%t", pass), ""))
				require_NoError(t, err)
				_, err = jsL.AddStream(strmCfg(fmt.Sprintf("fooL2-%t", pass), "clustL"))
				require_NoError(t, err)
				_, err = jsL.AddStream(strmCfg(fmt.Sprintf("fooL3-%t", pass), "clust1"))
				if pass {
					require_NoError(t, err)
				} else {
					require_Error(t, err)
					require_Contains(t, err.Error(), "no suitable peers for placement")
				}
			}
			clusterLnCnt := func(expected int) error {
				cnt := 0
				for _, s := range c.servers {
					cnt += s.NumLeafNodes()
				}
				if cnt == expected {
					return nil
				}
				return fmt.Errorf("not enought leaf node connections, got %d needed %d", cnt, expected)
			}

			// Even though there are two remotes defined in tmplL, only one will be able to connect.
			checkFor(t, 10*time.Second, time.Second/4, func() error { return clusterLnCnt(2) })
			checkLeafNodeConnectedCount(t, sLA, 1)
			checkLeafNodeConnectedCount(t, sLB, 1)
			c.waitOnPeerCount(2)

			if testCase.leafFunctionPreJoin {
				cl := cluster{t: t, servers: []*Server{sLA, sLB}}
				cl.waitOnLeader()
				cl.waitOnPeerCount(2)
				testJSFunctions(false)
			} else {
				// In cases where the leaf nodes have to wait for the system account to connect,
				// JetStream should not be operational during that time
				ncA := natsConnect(t, fmt.Sprintf("nats://a1:a1@127.0.0.1:%d", sLA.opts.Port))
				defer ncA.Close()
				jsA, err := ncA.JetStream()
				require_NoError(t, err)
				_, err = jsA.AddStream(strmCfg("fail-false", ""))
				require_Error(t, err)
			}
			// Starting the proxy will connect the system accounts.
			// After they are connected the clusters are merged.
			// Once this happened, all streams in test can be placed anywhere in the cluster.
			// Before that only the cluster the client is connected to can be used for placement
			p.start()

			// Even though there are two remotes defined in tmplL, only one will be able to connect.
			checkFor(t, 10*time.Second, time.Second/4, func() error { return clusterLnCnt(4) })
			checkLeafNodeConnectedCount(t, sLA, 2)
			checkLeafNodeConnectedCount(t, sLB, 2)

			// The leader will reside in the main cluster only
			c.waitOnPeerCount(4)
			testJSFunctions(true)
		})
	}
}

func TestJetStreamLeafNodeClusterMixedModeExtensionWithSystemAccount(t *testing.T) {
	/*  Topology used in this test:
	CLUSTER(A <-> B <-> C (NO JS))
	      	            ^
	                    |
	                    LA
	*/

	// once every server is up, we expect these peers to be part of the JetStream meta cluster
	expectedJetStreamPeers := map[string]struct{}{
		"A":  {},
		"B":  {},
		"LA": {},
	}

	tmplA := `
listen: 127.0.0.1:-1
accounts :{
    A:{   jetstream: enable, users:[ {user:a1,password:a1}]},
    SYS:{ users:[ {user:s1,password:s1}]},
}
system_account: SYS
leafnodes: {
	listen: 127.0.0.1:-1
	no_advertise: true
	authorization: {
		timeout: 0.5
	}
}
jetstream: { %s store_dir: '%s'; max_mem: 50Mb, max_file: 50Mb }
server_name: A
cluster: {
	name: clust1
	listen: 127.0.0.1:20114
	routes=[nats-route://127.0.0.1:20115,nats-route://127.0.0.1:20116]
	no_advertise: true
}
`

	tmplB := `
listen: 127.0.0.1:-1
accounts :{
    A:{   jetstream: enable, users:[ {user:a1,password:a1}]},
    SYS:{ users:[ {user:s1,password:s1}]},
}
system_account: SYS
leafnodes: {
	listen: 127.0.0.1:-1
	no_advertise: true
	authorization: {
		timeout: 0.5
	}
}
jetstream: { %s store_dir: '%s'; max_mem: 50Mb, max_file: 50Mb }
server_name: B
cluster: {
	name: clust1
	listen: 127.0.0.1:20115
	routes=[nats-route://127.0.0.1:20114,nats-route://127.0.0.1:20116]
	no_advertise: true
}
`

	tmplC := `
listen: 127.0.0.1:-1
accounts :{
    A:{   jetstream: enable, users:[ {user:a1,password:a1}]},
    SYS:{ users:[ {user:s1,password:s1}]},
}
system_account: SYS
leafnodes: {
	listen: 127.0.0.1:-1
	no_advertise: true
	authorization: {
		timeout: 0.5
	}
}
jetstream: {
	enabled: false
	%s
}
server_name: C
cluster: {
	name: clust1
	listen: 127.0.0.1:20116
	routes=[nats-route://127.0.0.1:20114,nats-route://127.0.0.1:20115]
	no_advertise: true
}
`

	tmplLA := `
listen: 127.0.0.1:-1
accounts :{
    A:{   jetstream: enable, users:[ {user:a1,password:a1}]},
    SYS:{ users:[ {user:s1,password:s1}]},
}
system_account = SYS
# the extension hint is to simplify this test. without it present we would need a cluster of size 2
jetstream: { %s store_dir: '%s'; max_mem: 50Mb, max_file: 50Mb, extension_hint: will_extend }
server_name: LA
leafnodes:{
	no_advertise: true
    remotes:[{url:nats://a1:a1@127.0.0.1:%d, account: A},
		     {url:nats://s1:s1@127.0.0.1:%d, account: SYS}]
}
# add the cluster here so we can test placement
cluster: { name: clustL }
`
	for _, withDomain := range []bool{true, false} {
		t.Run(fmt.Sprintf("with-domain:%t", withDomain), func(t *testing.T) {
			var jsDisabledDomainString string
			var jsEnabledDomainString string
			if withDomain {
				jsEnabledDomainString = `domain: "domain", `
				jsDisabledDomainString = `domain: "domain"`
			} else {
				// in case no domain name is set, fall back to the extension hint.
				// since JS is disabled, the value of this does not clash with other uses.
				jsDisabledDomainString = "extension_hint: will_extend"
			}

			sd1 := t.TempDir()
			confA := createConfFile(t, []byte(fmt.Sprintf(tmplA, jsEnabledDomainString, sd1)))
			sA, _ := RunServerWithConfig(confA)
			defer sA.Shutdown()

			sd2 := t.TempDir()
			confB := createConfFile(t, []byte(fmt.Sprintf(tmplB, jsEnabledDomainString, sd2)))
			sB, _ := RunServerWithConfig(confB)
			defer sB.Shutdown()

			confC := createConfFile(t, []byte(fmt.Sprintf(tmplC, jsDisabledDomainString)))
			sC, _ := RunServerWithConfig(confC)
			defer sC.Shutdown()

			checkClusterFormed(t, sA, sB, sC)
			c := cluster{t: t, servers: []*Server{sA, sB, sC}}
			c.waitOnPeerCount(2)

			sd3 := t.TempDir()
			// deliberately pick server sC (no JS) to connect to
			confLA := createConfFile(t, []byte(fmt.Sprintf(tmplLA, jsEnabledDomainString, sd3, sC.opts.LeafNode.Port, sC.opts.LeafNode.Port)))
			sLA, _ := RunServerWithConfig(confLA)
			defer sLA.Shutdown()

			checkLeafNodeConnectedCount(t, sC, 2)
			checkLeafNodeConnectedCount(t, sLA, 2)
			c.waitOnPeerCount(3)
			peers := c.leader().JetStreamClusterPeers()
			for _, peer := range peers {
				if _, ok := expectedJetStreamPeers[peer]; !ok {
					t.Fatalf("Found unexpected peer %q", peer)
				}
			}

			// helper to create stream config with uniqe name and subject
			cnt := 0
			strmCfg := func(placementCluster string) *nats.StreamConfig {
				name := fmt.Sprintf("s-%d", cnt)
				cnt++
				if placementCluster == "" {
					return &nats.StreamConfig{Name: name, Replicas: 1, Subjects: []string{name}}
				}
				return &nats.StreamConfig{Name: name, Replicas: 1, Subjects: []string{name},
					Placement: &nats.Placement{Cluster: placementCluster}}
			}

			test := func(port int, expectedDefPlacement string) {
				ncA := natsConnect(t, fmt.Sprintf("nats://a1:a1@127.0.0.1:%d", port))
				defer ncA.Close()
				jsA, err := ncA.JetStream()
				require_NoError(t, err)
				si, err := jsA.AddStream(strmCfg(""))
				require_NoError(t, err)
				require_Contains(t, si.Cluster.Name, expectedDefPlacement)
				si, err = jsA.AddStream(strmCfg("clust1"))
				require_NoError(t, err)
				require_Contains(t, si.Cluster.Name, "clust1")
				si, err = jsA.AddStream(strmCfg("clustL"))
				require_NoError(t, err)
				require_Contains(t, si.Cluster.Name, "clustL")
			}

			test(sA.opts.Port, "clust1")
			test(sB.opts.Port, "clust1")
			test(sC.opts.Port, "clust1")
			test(sLA.opts.Port, "clustL")
		})
	}
}

func TestJetStreamLeafNodeCredsDenies(t *testing.T) {
	tmplL := `
listen: 127.0.0.1:-1
accounts :{
    A:{   jetstream: enable, users:[ {user:a1,password:a1}]},
    SYS:{ users:[ {user:s1,password:s1}]},
}
system_account = SYS
jetstream: {
    domain: "cluster"
    store_dir: '%s'
    max_mem: 50Mb
    max_file: 50Mb
}
leafnodes:{
    remotes:[{url:nats://a1:a1@127.0.0.1:20125, account: A, credentials: '%s' },
		     {url:nats://s1:s1@127.0.0.1:20125, account: SYS, credentials: '%s', deny_imports: foo, deny_exports: bar}]
}
`
	akp, err := nkeys.CreateAccount()
	require_NoError(t, err)
	creds := createUserWithLimit(t, akp, time.Time{}, func(pl *jwt.UserPermissionLimits) {
		pl.Pub.Deny.Add(jsAllAPI)
		pl.Sub.Deny.Add(jsAllAPI)
	})

	sd := t.TempDir()

	confL := createConfFile(t, []byte(fmt.Sprintf(tmplL, sd, creds, creds)))
	opts := LoadConfig(confL)
	sL, err := NewServer(opts)
	require_NoError(t, err)

	l := captureNoticeLogger{}
	sL.SetLogger(&l, false, false)

	go sL.Start()
	defer sL.Shutdown()

	// wait till the notices got printed
UNTIL_READY:
	for {
		<-time.After(50 * time.Millisecond)
		l.Lock()
		for _, n := range l.notices {
			if strings.Contains(n, "Server is ready") {
				l.Unlock()
				break UNTIL_READY
			}
		}
		l.Unlock()
	}

	l.Lock()
	cnt := 0
	for _, n := range l.notices {
		if strings.Contains(n, "LeafNode Remote for Account A uses credentials file") ||
			strings.Contains(n, "LeafNode Remote for System Account uses") ||
			strings.Contains(n, "Remote for System Account uses restricted export permissions") ||
			strings.Contains(n, "Remote for System Account uses restricted import permissions") {
			cnt++
		}
	}
	l.Unlock()
	require_True(t, cnt == 4)
}

func TestJetStreamLeafNodeDefaultDomainCfg(t *testing.T) {
	tmplHub := `
listen: 127.0.0.1:%d
accounts :{
    A:{ jetstream: %s, users:[ {user:a1,password:a1}]},
    SYS:{ users:[ {user:s1,password:s1}]},
}
system_account: SYS
jetstream : %s
server_name: HUB
leafnodes: {
	listen: 127.0.0.1:%d
}
%s
`

	tmplL := `
listen: 127.0.0.1:-1
accounts :{
    A:{   jetstream: enable, users:[ {user:a1,password:a1}]},
    SYS:{ users:[ {user:s1,password:s1}]},
}
system_account: SYS
jetstream: { domain: "%s", store_dir: '%s', max_mem: 100Mb, max_file: 100Mb }
server_name: LEAF
leafnodes: {
    remotes:[{url:nats://a1:a1@127.0.0.1:%d, account: A},%s]
}
%s
`

	test := func(domain string, sysShared bool) {
		confHub := createConfFile(t, []byte(fmt.Sprintf(tmplHub, -1, "disabled", "disabled", -1, "")))
		sHub, _ := RunServerWithConfig(confHub)
		defer sHub.Shutdown()

		noDomainFix := ""
		if domain == _EMPTY_ {
			noDomainFix = `default_js_domain:{A:""}`
		}

		sys := ""
		if sysShared {
			sys = fmt.Sprintf(`{url:nats://s1:s1@127.0.0.1:%d, account: SYS}`, sHub.opts.LeafNode.Port)
		}

		sdLeaf := t.TempDir()
		confL := createConfFile(t, []byte(fmt.Sprintf(tmplL, domain, sdLeaf, sHub.opts.LeafNode.Port, sys, noDomainFix)))
		sLeaf, _ := RunServerWithConfig(confL)
		defer sLeaf.Shutdown()

		lnCnt := 1
		if sysShared {
			lnCnt++
		}

		checkLeafNodeConnectedCount(t, sHub, lnCnt)
		checkLeafNodeConnectedCount(t, sLeaf, lnCnt)

		ncA := natsConnect(t, fmt.Sprintf("nats://a1:a1@127.0.0.1:%d", sHub.opts.Port))
		defer ncA.Close()
		jsA, err := ncA.JetStream()
		require_NoError(t, err)

		_, err = jsA.AddStream(&nats.StreamConfig{Name: "foo", Replicas: 1, Subjects: []string{"foo"}})
		require_True(t, err == nats.ErrNoResponders)

		// Add in default domain and restart server
		require_NoError(t, os.WriteFile(confHub, []byte(fmt.Sprintf(tmplHub,
			sHub.opts.Port,
			"disabled",
			"disabled",
			sHub.opts.LeafNode.Port,
			fmt.Sprintf(`default_js_domain: {A:"%s"}`, domain))), 0664))

		sHub.Shutdown()
		sHub.WaitForShutdown()
		checkLeafNodeConnectedCount(t, sLeaf, 0)
		sHubUpd1, _ := RunServerWithConfig(confHub)
		defer sHubUpd1.Shutdown()

		checkLeafNodeConnectedCount(t, sHubUpd1, lnCnt)
		checkLeafNodeConnectedCount(t, sLeaf, lnCnt)

		_, err = jsA.AddStream(&nats.StreamConfig{Name: "foo", Replicas: 1, Subjects: []string{"foo"}})
		require_NoError(t, err)

		// Enable jetstream in hub.
		sdHub := t.TempDir()
		jsEnabled := fmt.Sprintf(`{ domain: "%s", store_dir: '%s', max_mem: 100Mb, max_file: 100Mb }`, domain, sdHub)
		require_NoError(t, os.WriteFile(confHub, []byte(fmt.Sprintf(tmplHub,
			sHubUpd1.opts.Port,
			"disabled",
			jsEnabled,
			sHubUpd1.opts.LeafNode.Port,
			fmt.Sprintf(`default_js_domain: {A:"%s"}`, domain))), 0664))

		sHubUpd1.Shutdown()
		sHubUpd1.WaitForShutdown()
		checkLeafNodeConnectedCount(t, sLeaf, 0)
		sHubUpd2, _ := RunServerWithConfig(confHub)
		defer sHubUpd2.Shutdown()

		checkLeafNodeConnectedCount(t, sHubUpd2, lnCnt)
		checkLeafNodeConnectedCount(t, sLeaf, lnCnt)

		_, err = jsA.AddStream(&nats.StreamConfig{Name: "bar", Replicas: 1, Subjects: []string{"bar"}})
		require_NoError(t, err)

		// Enable jetstream in account A of hub
		// This is a mis config, as you can't have it both ways, local jetstream but default to another one
		require_NoError(t, os.WriteFile(confHub, []byte(fmt.Sprintf(tmplHub,
			sHubUpd2.opts.Port,
			"enabled",
			jsEnabled,
			sHubUpd2.opts.LeafNode.Port,
			fmt.Sprintf(`default_js_domain: {A:"%s"}`, domain))), 0664))

		if domain != _EMPTY_ {
			// in case no domain name exists there are no additional guard rails, hence no error
			// It is the users responsibility to get this edge case right
			sHubUpd2.Shutdown()
			sHubUpd2.WaitForShutdown()
			checkLeafNodeConnectedCount(t, sLeaf, 0)
			sHubUpd3, err := NewServer(LoadConfig(confHub))
			sHubUpd3.Shutdown()

			require_Error(t, err)
			require_Contains(t, err.Error(), `default_js_domain contains account name "A" with enabled JetStream`)
		}
	}

	t.Run("with-domain-sys", func(t *testing.T) {
		test("domain", true)
	})
	t.Run("with-domain-nosys", func(t *testing.T) {
		test("domain", false)
	})
	t.Run("no-domain", func(t *testing.T) {
		test("", true)
	})
	t.Run("no-domain", func(t *testing.T) {
		test("", false)
	})
}

func TestJetStreamLeafNodeDefaultDomainJwtExplicit(t *testing.T) {
	tmplHub := `
listen: 127.0.0.1:%d
operator: %s
system_account: %s
resolver: MEM
resolver_preload: {
	%s:%s
	%s:%s
}
jetstream : disabled
server_name: HUB
leafnodes: {
	listen: 127.0.0.1:%d
}
%s
`

	tmplL := `
listen: 127.0.0.1:-1
accounts :{
    A:{   jetstream: enable, users:[ {user:a1,password:a1}]},
    SYS:{ users:[ {user:s1,password:s1}]},
}
system_account: SYS
jetstream: { domain: "%s", store_dir: '%s', max_mem: 100Mb, max_file: 100Mb }
server_name: LEAF
leafnodes: {
    remotes:[{url:nats://127.0.0.1:%d, account: A, credentials: '%s'},
		     {url:nats://127.0.0.1:%d, account: SYS, credentials: '%s'}]
}
%s
`

	test := func(domain string) {
		noDomainFix := ""
		if domain == _EMPTY_ {
			noDomainFix = `default_js_domain:{A:""}`
		}

		sysKp, syspub := createKey(t)
		sysJwt := encodeClaim(t, jwt.NewAccountClaims(syspub), syspub)
		sysCreds := newUser(t, sysKp)

		aKp, aPub := createKey(t)
		aClaim := jwt.NewAccountClaims(aPub)
		aJwt := encodeClaim(t, aClaim, aPub)
		aCreds := newUser(t, aKp)

		confHub := createConfFile(t, []byte(fmt.Sprintf(tmplHub, -1, ojwt, syspub, syspub, sysJwt, aPub, aJwt, -1, "")))
		sHub, _ := RunServerWithConfig(confHub)
		defer sHub.Shutdown()

		sdLeaf := t.TempDir()
		confL := createConfFile(t, []byte(fmt.Sprintf(tmplL,
			domain,
			sdLeaf,
			sHub.opts.LeafNode.Port,
			aCreds,
			sHub.opts.LeafNode.Port,
			sysCreds,
			noDomainFix)))
		sLeaf, _ := RunServerWithConfig(confL)
		defer sLeaf.Shutdown()

		checkLeafNodeConnectedCount(t, sHub, 2)
		checkLeafNodeConnectedCount(t, sLeaf, 2)

		ncA := natsConnect(t, fmt.Sprintf("nats://127.0.0.1:%d", sHub.opts.Port), createUserCreds(t, nil, aKp))
		defer ncA.Close()
		jsA, err := ncA.JetStream()
		require_NoError(t, err)

		_, err = jsA.AddStream(&nats.StreamConfig{Name: "foo", Replicas: 1, Subjects: []string{"foo"}})
		require_True(t, err == nats.ErrNoResponders)

		// Add in default domain and restart server
		require_NoError(t, os.WriteFile(confHub, []byte(fmt.Sprintf(tmplHub,
			sHub.opts.Port, ojwt, syspub, syspub, sysJwt, aPub, aJwt, sHub.opts.LeafNode.Port,
			fmt.Sprintf(`default_js_domain: {%s:"%s"}`, aPub, domain))), 0664))

		sHub.Shutdown()
		sHub.WaitForShutdown()
		checkLeafNodeConnectedCount(t, sLeaf, 0)
		sHubUpd1, _ := RunServerWithConfig(confHub)
		defer sHubUpd1.Shutdown()

		checkLeafNodeConnectedCount(t, sHubUpd1, 2)
		checkLeafNodeConnectedCount(t, sLeaf, 2)

		_, err = jsA.AddStream(&nats.StreamConfig{Name: "bar", Replicas: 1, Subjects: []string{"bar"}})
		require_NoError(t, err)
	}
	t.Run("with-domain", func(t *testing.T) {
		test("domain")
	})
	t.Run("no-domain", func(t *testing.T) {
		test("")
	})
}

func TestJetStreamLeafNodeDefaultDomainClusterBothEnds(t *testing.T) {
	// test to ensure that default domain functions when both ends of the leaf node connection are clusters
	tmplHub1 := `
listen: 127.0.0.1:-1
accounts :{
    A:{ jetstream: enabled, users:[ {user:a1,password:a1}]},
	B:{ jetstream: enabled, users:[ {user:b1,password:b1}]}
}
jetstream : { domain: "DHUB", store_dir: '%s', max_mem: 100Mb, max_file: 100Mb }
server_name: HUB1
cluster: {
	name: HUB
	listen: 127.0.0.1:20134
	routes=[nats-route://127.0.0.1:20135]
}
leafnodes: {
	listen:127.0.0.1:-1
}
`

	tmplHub2 := `
listen: 127.0.0.1:-1
accounts :{
    A:{ jetstream: enabled, users:[ {user:a1,password:a1}]},
	B:{ jetstream: enabled, users:[ {user:b1,password:b1}]}
}
jetstream : { domain: "DHUB", store_dir: '%s', max_mem: 100Mb, max_file: 100Mb }
server_name: HUB2
cluster: {
	name: HUB
	listen: 127.0.0.1:20135
	routes=[nats-route://127.0.0.1:20134]
}
leafnodes: {
	listen:127.0.0.1:-1
}
`

	tmplL1 := `
listen: 127.0.0.1:-1
accounts :{
    A:{ jetstream: enabled,  users:[ {user:a1,password:a1}]},
	B:{ jetstream: disabled, users:[ {user:b1,password:b1}]}
}
jetstream: { domain: "DLEAF", store_dir: '%s', max_mem: 100Mb, max_file: 100Mb }
server_name: LEAF1
cluster: {
	name: LEAF
	listen: 127.0.0.1:20136
	routes=[nats-route://127.0.0.1:20137]
}
leafnodes: {
    remotes:[{url:nats://a1:a1@127.0.0.1:%d, account: A},{url:nats://b1:b1@127.0.0.1:%d, account: B}]
}
default_js_domain: {B:"DHUB"}
`

	tmplL2 := `
listen: 127.0.0.1:-1
accounts :{
    A:{ jetstream: enabled,  users:[ {user:a1,password:a1}]},
	B:{ jetstream: disabled, users:[ {user:b1,password:b1}]}
}
jetstream: { domain: "DLEAF", store_dir: '%s', max_mem: 100Mb, max_file: 100Mb }
server_name: LEAF2
cluster: {
	name: LEAF
	listen: 127.0.0.1:20137
	routes=[nats-route://127.0.0.1:20136]
}
leafnodes: {
    remotes:[{url:nats://a1:a1@127.0.0.1:%d, account: A},{url:nats://b1:b1@127.0.0.1:%d, account: B}]
}
default_js_domain: {B:"DHUB"}
`

	sd1 := t.TempDir()
	confHub1 := createConfFile(t, []byte(fmt.Sprintf(tmplHub1, sd1)))
	sHub1, _ := RunServerWithConfig(confHub1)
	defer sHub1.Shutdown()

	sd2 := t.TempDir()
	confHub2 := createConfFile(t, []byte(fmt.Sprintf(tmplHub2, sd2)))
	sHub2, _ := RunServerWithConfig(confHub2)
	defer sHub2.Shutdown()

	checkClusterFormed(t, sHub1, sHub2)
	c1 := cluster{t: t, servers: []*Server{sHub1, sHub2}}
	c1.waitOnPeerCount(2)

	sd3 := t.TempDir()
	confLeaf1 := createConfFile(t, []byte(fmt.Sprintf(tmplL1, sd3, sHub1.getOpts().LeafNode.Port, sHub1.getOpts().LeafNode.Port)))
	sLeaf1, _ := RunServerWithConfig(confLeaf1)
	defer sLeaf1.Shutdown()

	confLeaf2 := createConfFile(t, []byte(fmt.Sprintf(tmplL2, sd3, sHub1.getOpts().LeafNode.Port, sHub1.getOpts().LeafNode.Port)))
	sLeaf2, _ := RunServerWithConfig(confLeaf2)
	defer sLeaf2.Shutdown()

	checkClusterFormed(t, sLeaf1, sLeaf2)
	c2 := cluster{t: t, servers: []*Server{sLeaf1, sLeaf2}}
	c2.waitOnPeerCount(2)

	checkLeafNodeConnectedCount(t, sHub1, 4)
	checkLeafNodeConnectedCount(t, sLeaf1, 2)
	checkLeafNodeConnectedCount(t, sLeaf2, 2)

	ncB := natsConnect(t, fmt.Sprintf("nats://b1:b1@127.0.0.1:%d", sLeaf1.getOpts().Port))
	defer ncB.Close()
	jsB1, err := ncB.JetStream()
	require_NoError(t, err)
	si, err := jsB1.AddStream(&nats.StreamConfig{Name: "foo", Replicas: 1, Subjects: []string{"foo"}})
	require_NoError(t, err)
	require_Equal(t, si.Cluster.Name, "HUB")

	jsB2, err := ncB.JetStream(nats.Domain("DHUB"))
	require_NoError(t, err)
	si, err = jsB2.AddStream(&nats.StreamConfig{Name: "bar", Replicas: 1, Subjects: []string{"bar"}})
	require_NoError(t, err)
	require_Equal(t, si.Cluster.Name, "HUB")
}

func TestJetStreamLeafNodeSvcImportExportCycle(t *testing.T) {
	accounts := `
	accounts {
		SYS: {
			users: [{user: admin, password: admin}]
		}
		LEAF_USER: {
			users: [{user: leaf_user, password: leaf_user}]
			imports: [
				{service: {account: LEAF_INGRESS, subject: "foo"}}
				{service: {account: LEAF_INGRESS, subject: "_INBOX.>"}}
				{service: {account: LEAF_INGRESS, subject: "$JS.leaf.API.>"}, to: "JS.leaf_ingress@leaf.API.>" }
			]
			jetstream: enabled
		}
		LEAF_INGRESS: {
			users: [{user: leaf_ingress, password: leaf_ingress}]
			exports: [
				{service: "foo", accounts: [LEAF_USER]}
				{service: "_INBOX.>", accounts: [LEAF_USER]}
				{service: "$JS.leaf.API.>", response_type: "stream", accounts: [LEAF_USER]}
			]
			imports: [
			]
			jetstream: enabled
		}
	}
	system_account: SYS
	`

	hconf := createConfFile(t, []byte(fmt.Sprintf(`
	%s
	listen: "127.0.0.1:-1"
	leafnodes {
		listen: "127.0.0.1:-1"
	}
	`, accounts)))
	defer os.Remove(hconf)
	s, o := RunServerWithConfig(hconf)
	defer s.Shutdown()

	lconf := createConfFile(t, []byte(fmt.Sprintf(`
	%s
	server_name: leaf-server
	jetstream {
		store_dir: '%s'
		domain=leaf
	}

	listen: "127.0.0.1:-1"
	leafnodes {
		remotes = [
			{
				urls: ["nats-leaf://leaf_ingress:leaf_ingress@127.0.0.1:%v"]
				account: "LEAF_INGRESS"
			}
		]
	}
	`, accounts, t.TempDir(), o.LeafNode.Port)))
	defer os.Remove(lconf)
	sl, so := RunServerWithConfig(lconf)
	defer sl.Shutdown()

	checkLeafNodeConnected(t, sl)

	nc := natsConnect(t, fmt.Sprintf("nats://leaf_user:leaf_user@127.0.0.1:%v", so.Port))
	defer nc.Close()

	js, _ := nc.JetStream(nats.APIPrefix("JS.leaf_ingress@leaf.API."))

	_, err := js.AddStream(&nats.StreamConfig{
		Name:     "TEST",
		Subjects: []string{"foo"},
		Storage:  nats.FileStorage,
	})
	require_NoError(t, err)

	_, err = js.Publish("foo", []byte("msg"))
	require_NoError(t, err)
}

func TestJetStreamLeafNodeJSClusterMigrateRecovery(t *testing.T) {
	tmpl := strings.Replace(jsClusterAccountsTempl, "store_dir:", "domain: hub, store_dir:", 1)
	c := createJetStreamCluster(t, tmpl, "hub", _EMPTY_, 3, 12232, true)
	defer c.shutdown()

	tmpl = strings.Replace(jsClusterTemplWithLeafNode, "store_dir:", "domain: leaf, store_dir:", 1)
	lnc := c.createLeafNodesWithTemplateAndStartPort(tmpl, "leaf", 3, 23913)
	defer lnc.shutdown()

	lnc.waitOnClusterReady()
	for _, s := range lnc.servers {
		s.setJetStreamMigrateOnRemoteLeaf()
	}

	nc, _ := jsClientConnect(t, lnc.randomServer())
	defer nc.Close()

	ljs, err := nc.JetStream(nats.Domain("leaf"))
	require_NoError(t, err)

	// Create an asset in the leafnode cluster.
	si, err := ljs.AddStream(&nats.StreamConfig{
		Name:     "TEST",
		Subjects: []string{"foo"},
		Replicas: 3,
	})
	require_NoError(t, err)
	require_Equal(t, si.Cluster.Name, "leaf")
	require_NotEqual(t, si.Cluster.Leader, noLeader)
	require_Equal(t, len(si.Cluster.Replicas), 2)

	// Count how many remotes each server in the leafnode cluster is
	// supposed to have and then take them down.
	remotes := map[*Server]int{}
	for _, s := range lnc.servers {
		remotes[s] += len(s.leafRemoteCfgs)
		s.closeAndDisableLeafnodes()
		checkLeafNodeConnectedCount(t, s, 0)
	}

	// The Raft nodes in the leafnode cluster now need some time to
	// notice that they're no longer receiving AEs from a leader, as
	// they should have been forced into observer mode. Check that
	// this is the case.
	time.Sleep(maxElectionTimeout)
	for _, s := range lnc.servers {
		s.rnMu.RLock()
		for name, n := range s.raftNodes {
			// We don't expect the metagroup to have turned into an
			// observer but all other assets should have done.
			if name == defaultMetaGroupName {
				require_False(t, n.IsObserver())
			} else {
				require_True(t, n.IsObserver())
			}
		}
		s.rnMu.RUnlock()
	}

	// Bring the leafnode connections back up.
	for _, s := range lnc.servers {
		s.reEnableLeafnodes()
		checkLeafNodeConnectedCount(t, s, remotes[s])
	}

	// Wait for nodes to notice they are no longer in observer mode
	// and to leave observer mode.
	time.Sleep(maxElectionTimeout)
	for _, s := range lnc.servers {
		s.rnMu.RLock()
		for _, n := range s.raftNodes {
			require_False(t, n.IsObserver())
		}
		s.rnMu.RUnlock()
	}

	// Previously nodes would have left observer mode but then would
	// have failed to elect a stream leader as they were stuck on a
	// long election timer. Now this should work reliably.
	lnc.waitOnStreamLeader(globalAccountName, "TEST")
}

func TestJetStreamLeafNodeJSClusterMigrateRecoveryWithDelay(t *testing.T) {
	tmpl := strings.Replace(jsClusterAccountsTempl, "store_dir:", "domain: hub, store_dir:", 1)
	c := createJetStreamCluster(t, tmpl, "hub", _EMPTY_, 3, 12232, true)
	defer c.shutdown()

	tmpl = strings.Replace(jsClusterTemplWithLeafNode, "store_dir:", "domain: leaf, store_dir:", 1)
	lnc := c.createLeafNodesWithTemplateAndStartPort(tmpl, "leaf", 3, 23913)
	defer lnc.shutdown()

	lnc.waitOnClusterReady()
	delay := 5 * time.Second
	for _, s := range lnc.servers {
		s.setJetStreamMigrateOnRemoteLeafWithDelay(delay)
	}

	nc, _ := jsClientConnect(t, lnc.randomServer())
	defer nc.Close()

	ljs, err := nc.JetStream(nats.Domain("leaf"))
	require_NoError(t, err)

	// Create an asset in the leafnode cluster.
	si, err := ljs.AddStream(&nats.StreamConfig{
		Name:     "TEST",
		Subjects: []string{"foo"},
		Replicas: 3,
	})
	require_NoError(t, err)
	require_Equal(t, si.Cluster.Name, "leaf")
	require_NotEqual(t, si.Cluster.Leader, noLeader)
	require_Equal(t, len(si.Cluster.Replicas), 2)

	// Count how many remotes each server in the leafnode cluster is
	// supposed to have and then take them down.
	remotes := map[*Server]int{}
	for _, s := range lnc.servers {
		remotes[s] += len(s.leafRemoteCfgs)
		s.closeAndDisableLeafnodes()
		checkLeafNodeConnectedCount(t, s, 0)
	}

	// The Raft nodes in the leafnode cluster now need some time to
	// notice that they're no longer receiving AEs from a leader, as
	// they should have been forced into observer mode. Check that
	// this is the case.
	// We expect the nodes to become observers after the delay time.
	now := time.Now()
	timeout := maxElectionTimeout + delay
	success := false
	for time.Since(now) <= timeout {
		allObservers := true
		for _, s := range lnc.servers {
			s.rnMu.RLock()
			for name, n := range s.raftNodes {
				if name == defaultMetaGroupName {
					require_False(t, n.IsObserver())
				} else if n.IsObserver() {
					// Make sure the migration delay is respected.
					require_True(t, time.Since(now) > time.Duration(float64(delay)*0.7))
				} else {
					allObservers = false
				}
			}
			s.rnMu.RUnlock()
		}
		if allObservers {
			success = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	require_True(t, success)

	// Bring the leafnode connections back up.
	for _, s := range lnc.servers {
		s.reEnableLeafnodes()
		checkLeafNodeConnectedCount(t, s, remotes[s])
	}

	// Wait for nodes to notice they are no longer in observer mode
	// and to leave observer mode.
	time.Sleep(maxElectionTimeout)
	for _, s := range lnc.servers {
		s.rnMu.RLock()
		for _, n := range s.raftNodes {
			require_False(t, n.IsObserver())
		}
		s.rnMu.RUnlock()
	}

	// Make sure all delay timers in remotes are disabled
	for _, s := range lnc.servers {
		for _, r := range s.leafRemoteCfgs {
			require_True(t, r.jsMigrateTimer == nil)
		}
	}

	// Previously nodes would have left observer mode but then would
	// have failed to elect a stream leader as they were stuck on a
	// long election timer. Now this should work reliably.
	lnc.waitOnStreamLeader(globalAccountName, "TEST")
}

// This will test that when a mirror or source construct is setup across a leafnode/domain
// that it will recover quickly once the LN is re-established regardless
// of backoff state of the internal consumer create.
func TestJetStreamLeafNodeAndMirrorResyncAfterConnectionDown(t *testing.T) {
	tmplA := `
		listen: -1
		server_name: tcm
		jetstream {
			store_dir: '%s',
			domain: TCM
		}
		accounts {
			JS { users = [ { user: "y", pass: "p" } ]; jetstream: true }
			$SYS { users = [ { user: "admin", pass: "s3cr3t!" } ] }
		}
		leaf { port: -1 }
    `
	confA := createConfFile(t, []byte(fmt.Sprintf(tmplA, t.TempDir())))
	sA, oA := RunServerWithConfig(confA)
	defer sA.Shutdown()

	// Create a proxy - we will use this to simulate a network down event.
	rtt, bw := 10*time.Microsecond, 10*1024*1024*1024
	proxy := newNetProxy(rtt, bw, bw, fmt.Sprintf("nats://y:p@127.0.0.1:%d", oA.LeafNode.Port))
	defer proxy.stop()

	tmplB := `
		listen: -1
		server_name: xmm
		jetstream {
			store_dir: '%s',
			domain: XMM
		}
		accounts {
			JS { users = [ { user: "y", pass: "p" } ]; jetstream: true }
			$SYS { users = [ { user: "admin", pass: "s3cr3t!" } ] }
		}
		leaf { remotes [ { url: %s, account: "JS" } ], reconnect: "0.25s" }
    `

	confB := createConfFile(t, []byte(fmt.Sprintf(tmplB, t.TempDir(), proxy.leafURL())))
	sB, _ := RunServerWithConfig(confB)
	defer sA.Shutdown()

	// Make sure we are connected ok.
	checkLeafNodeConnectedCount(t, sA, 1)
	checkLeafNodeConnectedCount(t, sB, 1)

	// We will have 3 streams that we will test for proper syncing after
	// the network is restored.
	//
	//  1. Mirror A --> B
	//  2. Mirror A <-- B
	//  3. Source A <-> B

	// Connect to sA.
	ncA, jsA := jsClientConnect(t, sA, nats.UserInfo("y", "p"))
	defer ncA.Close()

	// Connect to sB.
	ncB, jsB := jsClientConnect(t, sB, nats.UserInfo("y", "p"))
	defer ncB.Close()

	// Add in TEST-A
	_, err := jsA.AddStream(&nats.StreamConfig{Name: "TEST-A", Subjects: []string{"foo"}})
	require_NoError(t, err)

	// Add in TEST-B
	_, err = jsB.AddStream(&nats.StreamConfig{Name: "TEST-B", Subjects: []string{"bar"}})
	require_NoError(t, err)

	// Now setup mirrors.
	_, err = jsB.AddStream(&nats.StreamConfig{
		Name: "M-A",
		Mirror: &nats.StreamSource{
			Name:     "TEST-A",
			External: &nats.ExternalStream{APIPrefix: "$JS.TCM.API"},
		},
	})
	require_NoError(t, err)

	_, err = jsA.AddStream(&nats.StreamConfig{
		Name: "M-B",
		Mirror: &nats.StreamSource{
			Name:     "TEST-B",
			External: &nats.ExternalStream{APIPrefix: "$JS.XMM.API"},
		},
	})
	require_NoError(t, err)

	// Now add in the streams that will source from one another bi-directionally.
	_, err = jsA.AddStream(&nats.StreamConfig{
		Name:     "SRC-A",
		Subjects: []string{"A.*"},
		Sources: []*nats.StreamSource{{
			Name:          "SRC-B",
			FilterSubject: "B.*",
			External:      &nats.ExternalStream{APIPrefix: "$JS.XMM.API"},
		}},
	})
	require_NoError(t, err)

	_, err = jsB.AddStream(&nats.StreamConfig{
		Name:     "SRC-B",
		Subjects: []string{"B.*"},
		Sources: []*nats.StreamSource{{
			Name:          "SRC-A",
			FilterSubject: "A.*",
			External:      &nats.ExternalStream{APIPrefix: "$JS.TCM.API"},
		}},
	})
	require_NoError(t, err)

	// Now load them up with 500 messages.
	initMsgs := 500
	for i := 0; i < initMsgs; i++ {
		// Individual Streams
		jsA.PublishAsync("foo", []byte("PAYLOAD"))
		jsB.PublishAsync("bar", []byte("PAYLOAD"))
		// Bi-directional Sources
		jsA.PublishAsync("A.foo", []byte("PAYLOAD"))
		jsB.PublishAsync("B.bar", []byte("PAYLOAD"))
	}
	select {
	case <-jsA.PublishAsyncComplete():
	case <-time.After(5 * time.Second):
		t.Fatalf("Did not receive completion signal")
	}
	select {
	case <-jsB.PublishAsyncComplete():
	case <-time.After(5 * time.Second):
		t.Fatalf("Did not receive completion signal")
	}

	// Utility to check the number of stream msgs.
	checkStreamMsgs := func(js nats.JetStreamContext, sname string, expected int, perr error) error {
		t.Helper()
		if perr != nil {
			return perr
		}
		si, err := js.StreamInfo(sname)
		require_NoError(t, err)
		if si.State.Msgs != uint64(expected) {
			return fmt.Errorf("Expected %d msgs for %s, got state: %+v", expected, sname, si.State)
		}
		return nil
	}

	// Wait til we see all messages.
	checkFor(t, 2*time.Second, 250*time.Millisecond, func() error {
		err := checkStreamMsgs(jsA, "TEST-A", initMsgs, nil)
		err = checkStreamMsgs(jsB, "M-A", initMsgs, err)
		err = checkStreamMsgs(jsB, "TEST-B", initMsgs, err)
		err = checkStreamMsgs(jsA, "M-B", initMsgs, err)
		err = checkStreamMsgs(jsA, "SRC-A", initMsgs*2, err)
		err = checkStreamMsgs(jsB, "SRC-B", initMsgs*2, err)
		return err
	})

	// Take down proxy. This will stop any propagation of messages between TEST and M streams.
	proxy.stop()

	// Now add an additional 500 messages to originals on both sides.
	for i := 0; i < initMsgs; i++ {
		// Individual Streams
		jsA.PublishAsync("foo", []byte("PAYLOAD"))
		jsB.PublishAsync("bar", []byte("PAYLOAD"))
		// Bi-directional Sources
		jsA.PublishAsync("A.foo", []byte("PAYLOAD"))
		jsB.PublishAsync("B.bar", []byte("PAYLOAD"))
	}
	select {
	case <-jsA.PublishAsyncComplete():
	case <-time.After(5 * time.Second):
		t.Fatalf("Did not receive completion signal")
	}
	select {
	case <-jsB.PublishAsyncComplete():
	case <-time.After(5 * time.Second):
		t.Fatalf("Did not receive completion signal")
	}

	cancelAndDelayConsumer := func(s *Server, stream string) {
		// Now make sure internal consumer is at max backoff.
		acc, err := s.lookupAccount("JS")
		require_NoError(t, err)
		mset, err := acc.lookupStream(stream)
		require_NoError(t, err)

		// Reset sourceInfo to have lots of failures and last attempt 2 minutes ago.
		// Lock should be held on parent stream.
		resetSourceInfo := func(si *sourceInfo) {
			// Do not reset sip here to make sure that the internal logic clears.
			si.fails = 100
			si.lreq = time.Now().Add(-2 * time.Minute)
		}

		// Force the consumer to be canceled and we simulate 100 failed attempts
		// such that the next time we will try will be a long way out.
		mset.mu.Lock()
		if mset.mirror != nil {
			resetSourceInfo(mset.mirror)
			mset.cancelSourceInfo(mset.mirror)
			mset.scheduleSetupMirrorConsumerRetry()
		} else if len(mset.sources) > 0 {
			for iname, si := range mset.sources {
				resetSourceInfo(si)
				mset.cancelSourceInfo(si)
				mset.setupSourceConsumer(iname, si.sseq+1, time.Time{})
			}
		}
		mset.mu.Unlock()
	}

	// Mirrors
	cancelAndDelayConsumer(sA, "M-B")
	cancelAndDelayConsumer(sB, "M-A")
	// Now bi-directional sourcing
	cancelAndDelayConsumer(sA, "SRC-A")
	cancelAndDelayConsumer(sB, "SRC-B")

	// Now restart the network proxy.
	proxy.start()

	// Make sure we are connected ok.
	checkLeafNodeConnectedCount(t, sA, 1)
	checkLeafNodeConnectedCount(t, sB, 1)

	// These should be good before re-sync.
	require_NoError(t, checkStreamMsgs(jsA, "TEST-A", initMsgs*2, nil))
	require_NoError(t, checkStreamMsgs(jsB, "TEST-B", initMsgs*2, nil))

	start := time.Now()
	// Wait til we see all messages.
	checkFor(t, 2*time.Minute, 50*time.Millisecond, func() error {
		err := checkStreamMsgs(jsA, "M-B", initMsgs*2, err)
		err = checkStreamMsgs(jsB, "M-A", initMsgs*2, err)
		err = checkStreamMsgs(jsA, "SRC-A", initMsgs*4, err)
		err = checkStreamMsgs(jsB, "SRC-B", initMsgs*4, err)
		return err
	})
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Expected to resync all streams <3s but got %v", elapsed)
	}
}

// This test will test a 3 node setup where we have a hub node, a gateway node, and a satellite node.
// This is specifically testing re-sync when there is not a direct Domain with JS match for the first
// hop connect LN that is signaling.
//
//		  HUB <---- GW(+JS/DOMAIN) -----> SAT1
//		   ^
//		   |
//	       +------- GW(-JS/NO DOMAIN) --> SAT2
//
// The Gateway node will solicit the satellites but will act as a LN hub.
func TestJetStreamLeafNodeAndMirrorResyncAfterLeafEstablished(t *testing.T) {
	accs := `
		accounts {
			JS { users = [ { user: "u", pass: "p" } ]; jetstream: true }
			$SYS { users = [ { user: "admin", pass: "s3cr3t!" } ] }
		}
	`
	hubT := `
		listen: -1
		server_name: hub
		jetstream { store_dir: '%s', domain: HUB }
		%s
		leaf { port: -1 }
    `
	confA := createConfFile(t, []byte(fmt.Sprintf(hubT, t.TempDir(), accs)))
	sHub, oHub := RunServerWithConfig(confA)
	defer sHub.Shutdown()

	// We run the SAT node second to extract out info for solicitation from targeted GW.
	sat1T := `
		listen: -1
		server_name: sat1
		jetstream { store_dir: '%s', domain: SAT1 }
		%s
		leaf { port: -1 }
    `
	confB := createConfFile(t, []byte(fmt.Sprintf(sat1T, t.TempDir(), accs)))
	sSat1, oSat1 := RunServerWithConfig(confB)
	defer sSat1.Shutdown()

	sat2T := `
		listen: -1
		server_name: sat2
		jetstream { store_dir: '%s', domain: SAT2 }
		%s
		leaf { port: -1 }
    `
	confC := createConfFile(t, []byte(fmt.Sprintf(sat2T, t.TempDir(), accs)))
	sSat2, oSat2 := RunServerWithConfig(confC)
	defer sSat2.Shutdown()

	hubLeafPort := fmt.Sprintf("nats://u:p@127.0.0.1:%d", oHub.LeafNode.Port)
	sat1LeafPort := fmt.Sprintf("nats://u:p@127.0.0.1:%d", oSat1.LeafNode.Port)
	sat2LeafPort := fmt.Sprintf("nats://u:p@127.0.0.1:%d", oSat2.LeafNode.Port)

	gw1T := `
		listen: -1
		server_name: gw1
		jetstream { store_dir: '%s', domain: GW }
		%s
		leaf { remotes [ { url: %s, account: "JS" }, { url: %s, account: "JS", hub: true } ], reconnect: "0.25s" }
    `
	confD := createConfFile(t, []byte(fmt.Sprintf(gw1T, t.TempDir(), accs, hubLeafPort, sat1LeafPort)))
	sGW1, _ := RunServerWithConfig(confD)
	defer sGW1.Shutdown()

	gw2T := `
		listen: -1
		server_name: gw2
		accounts {
			JS { users = [ { user: "u", pass: "p" } ] }
			$SYS { users = [ { user: "admin", pass: "s3cr3t!" } ] }
		}
		leaf { remotes [ { url: %s, account: "JS" }, { url: %s, account: "JS", hub: true } ], reconnect: "0.25s" }
    `
	confE := createConfFile(t, []byte(fmt.Sprintf(gw2T, hubLeafPort, sat2LeafPort)))
	sGW2, _ := RunServerWithConfig(confE)
	defer sGW2.Shutdown()

	// Make sure we are connected ok.
	checkLeafNodeConnectedCount(t, sHub, 2)
	checkLeafNodeConnectedCount(t, sSat1, 1)
	checkLeafNodeConnectedCount(t, sSat2, 1)
	checkLeafNodeConnectedCount(t, sGW1, 2)
	checkLeafNodeConnectedCount(t, sGW2, 2)

	// Let's place a muxed stream on the hub and have it source from a stream on the Satellite.
	// Connect to Hub.
	ncHub, jsHub := jsClientConnect(t, sHub, nats.UserInfo("u", "p"))
	defer ncHub.Close()

	_, err := jsHub.AddStream(&nats.StreamConfig{Name: "HUB", Subjects: []string{"H.>"}})
	require_NoError(t, err)

	// Connect to Sat1.
	ncSat1, jsSat1 := jsClientConnect(t, sSat1, nats.UserInfo("u", "p"))
	defer ncSat1.Close()

	_, err = jsSat1.AddStream(&nats.StreamConfig{
		Name:     "SAT-1",
		Subjects: []string{"S1.*"},
		Sources: []*nats.StreamSource{{
			Name:          "HUB",
			FilterSubject: "H.SAT-1.>",
			External:      &nats.ExternalStream{APIPrefix: "$JS.HUB.API"},
		}},
	})
	require_NoError(t, err)

	// Connect to Sat2.
	ncSat2, jsSat2 := jsClientConnect(t, sSat2, nats.UserInfo("u", "p"))
	defer ncSat2.Close()

	_, err = jsSat2.AddStream(&nats.StreamConfig{
		Name:     "SAT-2",
		Subjects: []string{"S2.*"},
		Sources: []*nats.StreamSource{{
			Name:          "HUB",
			FilterSubject: "H.SAT-2.>",
			External:      &nats.ExternalStream{APIPrefix: "$JS.HUB.API"},
		}},
	})
	require_NoError(t, err)

	// Put in 10 msgs each in for each satellite.
	for i := 0; i < 10; i++ {
		jsHub.Publish("H.SAT-1.foo", []byte("CMD"))
		jsHub.Publish("H.SAT-2.foo", []byte("CMD"))
	}
	// Make sure both are sync'd.
	checkFor(t, time.Second, 100*time.Millisecond, func() error {
		si, err := jsSat1.StreamInfo("SAT-1")
		require_NoError(t, err)
		if si.State.Msgs != 10 {
			return errors.New("SAT-1 Not sync'd yet")
		}
		si, err = jsSat2.StreamInfo("SAT-2")
		require_NoError(t, err)
		if si.State.Msgs != 10 {
			return errors.New("SAT-2 Not sync'd yet")
		}
		return nil
	})

	testReconnect := func(t *testing.T, delay time.Duration, expected uint64) {
		// Now disconnect Sat1 and Sat2. In 2.12 we can do this with active: false, but since this will be
		// pulled into 2.11.9 just shutdown both gateways.
		sGW1.Shutdown()
		checkLeafNodeConnectedCount(t, sSat1, 0)
		checkLeafNodeConnectedCount(t, sHub, 1)

		sGW2.Shutdown()
		checkLeafNodeConnectedCount(t, sSat2, 0)
		checkLeafNodeConnectedCount(t, sHub, 0)

		// Send 10 more messages for each while GW1 and GW2 are down.
		for i := 0; i < 10; i++ {
			jsHub.Publish("H.SAT-1.foo", []byte("CMD"))
			jsHub.Publish("H.SAT-2.foo", []byte("CMD"))
		}

		// Keep GWs down for delay.
		time.Sleep(delay)

		sGW1, _ = RunServerWithConfig(confD)
		// Make sure we are connected ok.
		checkLeafNodeConnectedCount(t, sHub, 1)
		checkLeafNodeConnectedCount(t, sSat1, 1)
		checkLeafNodeConnectedCount(t, sGW1, 2)

		sGW2, _ = RunServerWithConfig(confE)
		// Make sure we are connected ok.
		checkLeafNodeConnectedCount(t, sHub, 2)
		checkLeafNodeConnectedCount(t, sSat2, 1)
		checkLeafNodeConnectedCount(t, sGW2, 2)

		// Make sure sync'd in less than a second or two.
		checkFor(t, 2*time.Second, 100*time.Millisecond, func() error {
			si, err := jsSat1.StreamInfo("SAT-1")
			require_NoError(t, err)
			if si.State.Msgs != expected {
				return fmt.Errorf("SAT-1 not sync'd, expected %d got %d", expected, si.State.Msgs)
			}
			si, err = jsSat2.StreamInfo("SAT-2")
			require_NoError(t, err)
			if si.State.Msgs != expected {
				return fmt.Errorf("SAT-2 not sync'd, expected %d got %d", expected, si.State.Msgs)
			}
			return nil
		})
	}

	// We will test two scenarios with amount of time the GWs (link) is down.
	// 1. Just a second, we will not have detected the consumer is offline as of yet.
	// 2. Just over sourceHealthCheckInterval, meaning we detect it is down and schedule for another try.
	t.Run(fmt.Sprintf("reconnect-%v", time.Second), func(t *testing.T) {
		testReconnect(t, time.Second, 20)
	})
	t.Run(fmt.Sprintf("reconnect-%v", sourceHealthCheckInterval+time.Second), func(t *testing.T) {
		testReconnect(t, sourceHealthCheckInterval+time.Second, 30)
	})
	defer sGW1.Shutdown()
	defer sGW2.Shutdown()
}
