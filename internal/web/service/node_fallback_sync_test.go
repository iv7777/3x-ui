package service

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/mhsanaei/3x-ui/v3/internal/database"
	"github.com/mhsanaei/3x-ui/v3/internal/database/model"
	"github.com/mhsanaei/3x-ui/v3/internal/web/runtime"
)

const tlsTCPStream = `{"network":"tcp","security":"tls","tlsSettings":{"serverName":"s"}}`

func fallbackSet(rules ...model.FallbackWire) *model.FallbackSet {
	set := model.FallbackSet(rules)
	if set == nil {
		set = model.FallbackSet{}
	}
	return &set
}

func seedNode(t *testing.T) int {
	t.Helper()
	node := &model.Node{Name: "fb-node", Address: "10.0.0.9", Port: 2053, ApiToken: "t", Enable: true, Status: "online"}
	if err := database.GetDB().Create(node).Error; err != nil {
		t.Fatalf("create node: %v", err)
	}
	return node.Id
}

func seedCentralVless(t *testing.T, nodeID int, tag string, port int, settings string) *model.Inbound {
	t.Helper()
	ib := &model.Inbound{
		UserId: 1, NodeID: &nodeID, Tag: tag, Enable: true, Port: port,
		Protocol: model.VLESS, Settings: settings, StreamSettings: tlsTCPStream,
	}
	if err := database.GetDB().Create(ib).Error; err != nil {
		t.Fatalf("create inbound %s: %v", tag, err)
	}
	return ib
}

// seedMasterRules stores rules as an operator would, then settles the push
// SetByMaster queues so the next sync is a clean (non-dirty) one.
func seedMasterRules(t *testing.T, master *model.Inbound, items ...FallbackInput) {
	t.Helper()
	if err := (&FallbackService{}).SetByMaster(master.Id, items); err != nil {
		t.Fatalf("seed master rules: %v", err)
	}
	if err := database.GetDB().Model(&model.Node{}).Where("id = ?", *master.NodeID).Update("config_dirty", false).Error; err != nil {
		t.Fatalf("settle node: %v", err)
	}
}

func fallbackRows(t *testing.T, masterID int) []model.InboundFallback {
	t.Helper()
	rows, err := (&FallbackService{}).GetByMaster(masterID)
	if err != nil {
		t.Fatalf("load fallback rows: %v", err)
	}
	return rows
}

func snapshotOf(tag string, port int, settings string, fallbacks *model.FallbackSet) *runtime.TrafficSnapshot {
	return &runtime.TrafficSnapshot{Inbounds: []*model.Inbound{{
		Tag: tag, Enable: true, Port: port, Protocol: model.VLESS,
		Settings: settings, StreamSettings: tlsTCPStream, Fallbacks: fallbacks,
	}}}
}

// Fallbacks created on a node live only in the node's inbound_fallbacks table;
// the master used to adopt the node's settings blob and never saw them.
func TestSetRemoteTraffic_AdoptsNodeFallbacks(t *testing.T) {
	setupConflictDB(t)
	nodeID := seedNode(t)
	central := seedCentralVless(t, nodeID, "in-443", 443, `{"clients":[]}`)

	snap := snapshotOf("in-443", 443, `{"clients":[]}`, fallbackSet(
		model.FallbackWire{Dest: "127.0.0.1:8081", Path: "/ws", Alpn: "h2", Name: "web", Xver: 1},
		model.FallbackWire{Dest: "10.1.1.1:9000"},
	))
	if _, err := (&InboundService{}).setRemoteTrafficLocked(nodeID, snap, false, false); err != nil {
		t.Fatalf("setRemoteTrafficLocked: %v", err)
	}

	rows := fallbackRows(t, central.Id)
	if len(rows) != 2 {
		t.Fatalf("adopted %d fallback rows, want 2: %+v", len(rows), rows)
	}
	if got, want := rows[0], (model.InboundFallback{
		Id: rows[0].Id, MasterId: central.Id, Dest: "127.0.0.1:8081", Path: "/ws", Alpn: "h2", Name: "web", Xver: 1, SortOrder: 0,
	}); got != want {
		t.Errorf("first row = %+v, want %+v", got, want)
	}
	if rows[1].Dest != "10.1.1.1:9000" || rows[1].SortOrder != 1 {
		t.Errorf("second row = %+v, want dest 10.1.1.1:9000 at sort order 1", rows[1])
	}
}

// A pending master edit wins: while the node is config-dirty the node's
// snapshot must not replace the master's rules.
func TestSetRemoteTraffic_DirtyKeepsMasterFallbacks(t *testing.T) {
	setupConflictDB(t)
	nodeID := seedNode(t)
	central := seedCentralVless(t, nodeID, "in-443", 443, `{"clients":[]}`)
	seedMasterRules(t, central, FallbackInput{Dest: "8080"})

	snap := snapshotOf("in-443", 443, `{"clients":[]}`, fallbackSet(model.FallbackWire{Dest: "9999"}))
	if _, err := (&InboundService{}).setRemoteTrafficLocked(nodeID, snap, true, false); err != nil {
		t.Fatalf("setRemoteTrafficLocked: %v", err)
	}

	rows := fallbackRows(t, central.Id)
	if len(rows) != 1 || rows[0].Dest != "8080" {
		t.Fatalf("dirty sync replaced the master's rules: %+v", rows)
	}
}

// An old-build node does not report the field at all; that must not read as
// "the node has no fallbacks" and wipe the master's rules.
func TestSetRemoteTraffic_NodeWithoutFallbackFieldKeepsMasterRules(t *testing.T) {
	setupConflictDB(t)
	nodeID := seedNode(t)
	central := seedCentralVless(t, nodeID, "in-443", 443, `{"clients":[]}`)
	seedMasterRules(t, central, FallbackInput{Dest: "8080"})

	snap := snapshotOf("in-443", 443, `{"clients":[]}`, nil)
	if _, err := (&InboundService{}).setRemoteTrafficLocked(nodeID, snap, false, false); err != nil {
		t.Fatalf("setRemoteTrafficLocked: %v", err)
	}

	if rows := fallbackRows(t, central.Id); len(rows) != 1 {
		t.Fatalf("rows after sync from an old node = %+v, want the master's one rule", rows)
	}
}

// A node that deleted its last fallback reports an empty set, which is a real
// change the master has to mirror.
func TestSetRemoteTraffic_EmptyNodeSetClearsMasterRules(t *testing.T) {
	setupConflictDB(t)
	nodeID := seedNode(t)
	central := seedCentralVless(t, nodeID, "in-443", 443, `{"clients":[]}`)
	seedMasterRules(t, central, FallbackInput{Dest: "8080"})

	snap := snapshotOf("in-443", 443, `{"clients":[]}`, fallbackSet())
	if _, err := (&InboundService{}).setRemoteTrafficLocked(nodeID, snap, false, false); err != nil {
		t.Fatalf("setRemoteTrafficLocked: %v", err)
	}

	if rows := fallbackRows(t, central.Id); len(rows) != 0 {
		t.Fatalf("rows after the node cleared its fallbacks = %+v, want none", rows)
	}
}

// A sync reporting the same resolved destination must leave a child-linked rule alone;
// two inbounds share it, so a rewrite could not recover the link.
func TestSetRemoteTraffic_UnchangedFallbacksKeepChildLinkedRows(t *testing.T) {
	setupConflictDB(t)
	nodeID := seedNode(t)
	central := seedCentralVless(t, nodeID, "in-443", 443, `{"clients":[]}`)
	child := seedCentralVless(t, nodeID, "in-8081", 8081, `{"clients":[]}`)
	twin := seedCentralVless(t, nodeID, "in-8081-twin", 8081, `{"clients":[]}`)
	for _, ib := range []*model.Inbound{child, twin} {
		ib.Listen = "127.0.0.1"
		if err := database.GetDB().Save(ib).Error; err != nil {
			t.Fatalf("save %s: %v", ib.Tag, err)
		}
	}
	seedMasterRules(t, central, FallbackInput{ChildId: child.Id, Path: "/p"})

	snap := snapshotOf("in-443", 443, `{"clients":[]}`, fallbackSet(model.FallbackWire{Dest: "127.0.0.1:8081", Path: "/p"}))
	for _, tag := range []string{"in-8081", "in-8081-twin"} {
		snap.Inbounds = append(snap.Inbounds, &model.Inbound{
			Tag: tag, Enable: true, Port: 8081, Listen: "127.0.0.1", Protocol: model.VLESS,
			Settings: `{"clients":[]}`, StreamSettings: tlsTCPStream, Fallbacks: fallbackSet(),
		})
	}
	if _, err := (&InboundService{}).setRemoteTrafficLocked(nodeID, snap, false, false); err != nil {
		t.Fatalf("setRemoteTrafficLocked: %v", err)
	}

	rows := fallbackRows(t, central.Id)
	if len(rows) != 1 || rows[0].ChildId != child.Id || rows[0].Dest != "" {
		t.Fatalf("rows = %+v, want the one rule still linked to child %d", rows, child.Id)
	}
}

// A rule the node points at a sibling inbound is linked back to that inbound,
// so client links of the child still advertise the master's endpoint.
func TestSetRemoteTraffic_AdoptedFallbackLinksToSiblingInbound(t *testing.T) {
	setupConflictDB(t)
	nodeID := seedNode(t)
	central := seedCentralVless(t, nodeID, "in-443", 443, `{"clients":[]}`)
	child := seedCentralVless(t, nodeID, "in-8081", 8081, `{"clients":[]}`)

	snap := snapshotOf("in-443", 443, `{"clients":[]}`, fallbackSet(model.FallbackWire{Dest: "127.0.0.1:8081"}))
	if _, err := (&InboundService{}).setRemoteTrafficLocked(nodeID, snap, false, false); err != nil {
		t.Fatalf("setRemoteTrafficLocked: %v", err)
	}

	rows := fallbackRows(t, central.Id)
	if len(rows) != 1 || rows[0].ChildId != child.Id || rows[0].Dest != "" {
		t.Fatalf("adopted rows = %+v, want one row linked to child %d with no literal dest", rows, child.Id)
	}
}

// settings.fallbacks was the carrier of the #5963 fix and came back from the
// node inside its raw settings; stored on the master it outlived the rules.
func TestSetRemoteTraffic_DoesNotStoreFallbacksKeyInCentralSettings(t *testing.T) {
	setupConflictDB(t)
	nodeID := seedNode(t)
	central := seedCentralVless(t, nodeID, "in-443", 443, `{"clients":[],"fallbacks":[{"dest":"8080"}]}`)

	snap := snapshotOf("in-443", 443, `{"clients":[],"fallbacks":[{"dest":"8080"}]}`, nil)
	if _, err := (&InboundService{}).setRemoteTrafficLocked(nodeID, snap, false, false); err != nil {
		t.Fatalf("setRemoteTrafficLocked: %v", err)
	}

	var got model.Inbound
	if err := database.GetDB().First(&got, central.Id).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	var settings map[string]any
	if err := json.Unmarshal([]byte(got.Settings), &settings); err != nil {
		t.Fatalf("central settings not json: %v", err)
	}
	if _, has := settings["fallbacks"]; has {
		t.Fatalf("central settings still carry fallbacks: %s", got.Settings)
	}
}

// An inbound adopted for the first time brings the node's rules along.
func TestSetRemoteTraffic_NewInboundAdoptsFallbacks(t *testing.T) {
	setupConflictDB(t)
	nodeID := seedNode(t)

	snap := snapshotOf("in-443", 443, `{"clients":[]}`, fallbackSet(model.FallbackWire{Dest: "8080"}))
	if _, err := (&InboundService{}).setRemoteTrafficLocked(nodeID, snap, false, false); err != nil {
		t.Fatalf("setRemoteTrafficLocked: %v", err)
	}

	var adopted model.Inbound
	if err := database.GetDB().Where("tag = ?", "in-443").First(&adopted).Error; err != nil {
		t.Fatalf("adopted inbound missing: %v", err)
	}
	if rows := fallbackRows(t, adopted.Id); len(rows) != 1 || rows[0].Dest != "8080" {
		t.Fatalf("rows on a freshly adopted inbound = %+v, want dest 8080", rows)
	}
}

// Editing a node inbound's rules on the master queues a push; without the
// dirty mark the node would never be told.
func TestSetByMaster_MarksNodeDirty(t *testing.T) {
	setupConflictDB(t)
	nodeID := seedNode(t)
	central := seedCentralVless(t, nodeID, "in-443", 443, `{"clients":[]}`)

	if err := (&FallbackService{}).SetByMaster(central.Id, []FallbackInput{{Dest: "8080"}}); err != nil {
		t.Fatalf("SetByMaster: %v", err)
	}

	var node model.Node
	if err := database.GetDB().First(&node, nodeID).Error; err != nil {
		t.Fatalf("reload node: %v", err)
	}
	if !node.ConfigDirty {
		t.Fatal("node not marked config-dirty after editing its inbound's fallbacks")
	}
}

// The wire payload to a node reports the full rule set, empty included, so a
// cleared list reaches the node; incapable inbounds report nothing.
func TestBuildInboundForNodePush_ReportsFallbackSet(t *testing.T) {
	setupConflictDB(t)
	nodeID := seedNode(t)
	svc := &InboundService{}
	db := database.GetDB()
	central := seedCentralVless(t, nodeID, "in-443", 443, `{"clients":[]}`)

	built, err := svc.buildInboundForNodePush(db, central)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if built.Fallbacks == nil || len(*built.Fallbacks) != 0 {
		t.Fatalf("no rules: Fallbacks = %v, want a non-nil empty set", built.Fallbacks)
	}

	if err := (&FallbackService{}).SetByMaster(central.Id, []FallbackInput{{Dest: "8080", Path: "/x"}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	built, err = svc.buildInboundForNodePush(db, central)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if want := fallbackSet(model.FallbackWire{Dest: "8080", Path: "/x"}); !reflect.DeepEqual(built.Fallbacks, want) {
		t.Fatalf("Fallbacks = %+v, want %+v", built.Fallbacks, want)
	}

	central.StreamSettings = `{"network":"ws","security":"none"}`
	built, err = svc.buildInboundForNodePush(db, central)
	if err != nil {
		t.Fatalf("build ws: %v", err)
	}
	if built.Fallbacks != nil {
		t.Fatalf("ws inbound reports Fallbacks = %+v, want none", built.Fallbacks)
	}
}

// The node lists its rules next to each inbound, resolved so no node-local
// inbound id leaves the node.
func TestGetInbounds_ReportsResolvedFallbacks(t *testing.T) {
	setupConflictDB(t)
	db := database.GetDB()
	master := &model.Inbound{UserId: 1, Tag: "in-443", Enable: true, Port: 443, Protocol: model.VLESS, Settings: `{"clients":[]}`, StreamSettings: tlsTCPStream}
	child := &model.Inbound{UserId: 1, Tag: "in-8081", Enable: true, Port: 8081, Listen: "0.0.0.0", Protocol: model.VLESS, Settings: `{"clients":[]}`, StreamSettings: tlsTCPStream}
	for _, ib := range []*model.Inbound{master, child} {
		if err := db.Create(ib).Error; err != nil {
			t.Fatalf("create %s: %v", ib.Tag, err)
		}
	}
	if err := (&FallbackService{}).SetByMaster(master.Id, []FallbackInput{{ChildId: child.Id, Path: "/p"}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	list, err := (&InboundService{}).GetInbounds(1)
	if err != nil {
		t.Fatalf("GetInbounds: %v", err)
	}
	byTag := map[string]*model.Inbound{}
	for _, ib := range list {
		byTag[ib.Tag] = ib
	}
	if want := fallbackSet(model.FallbackWire{Dest: "127.0.0.1:8081", Path: "/p"}); !reflect.DeepEqual(byTag["in-443"].Fallbacks, want) {
		t.Errorf("master Fallbacks = %+v, want %+v", byTag["in-443"].Fallbacks, want)
	}
	if got := byTag["in-8081"].Fallbacks; got == nil || len(*got) != 0 {
		t.Errorf("child Fallbacks = %v, want a non-nil empty set", got)
	}
}

// The master pushes its rules; the node stores them as its own table rows (so
// its UI shows what it runs) and drops the settings.fallbacks copy.
func TestUpdateInbound_AppliesPushedFallbackSet(t *testing.T) {
	setupConflictDB(t)
	svc := &InboundService{}
	node := &model.Inbound{UserId: 1, Tag: "in-443", Enable: true, Port: 443, Protocol: model.VLESS, Settings: `{"clients":[]}`, StreamSettings: tlsTCPStream}
	if err := database.GetDB().Create(node).Error; err != nil {
		t.Fatalf("create: %v", err)
	}

	push := func(settings string, set *model.FallbackSet) *model.Inbound {
		t.Helper()
		in := &model.Inbound{
			Id: node.Id, UserId: 1, Tag: "in-443", Enable: true, Port: 443, Protocol: model.VLESS,
			Settings: settings, StreamSettings: tlsTCPStream, Fallbacks: set,
		}
		if _, _, err := svc.UpdateInbound(in); err != nil {
			t.Fatalf("UpdateInbound: %v", err)
		}
		var stored model.Inbound
		if err := database.GetDB().First(&stored, node.Id).Error; err != nil {
			t.Fatalf("reload: %v", err)
		}
		return &stored
	}

	stored := push(`{"clients":[],"fallbacks":[{"dest":"8080"}]}`, fallbackSet(model.FallbackWire{Dest: "8080", Path: "/a"}))
	if rows := fallbackRows(t, node.Id); len(rows) != 1 || rows[0].Dest != "8080" || rows[0].Path != "/a" {
		t.Fatalf("rows after push = %+v", rows)
	}
	if got := stored.Settings; json.Valid([]byte(got)) {
		var m map[string]any
		_ = json.Unmarshal([]byte(got), &m)
		if _, has := m["fallbacks"]; has {
			t.Errorf("stored settings keep a fallbacks copy: %s", got)
		}
	}

	rowID := fallbackRows(t, node.Id)[0].Id
	push(`{"clients":[]}`, fallbackSet(model.FallbackWire{Dest: "8080", Path: "/a"}))
	if rows := fallbackRows(t, node.Id); len(rows) != 1 || rows[0].Id != rowID {
		t.Errorf("identical push rewrote the rows: %+v (first id was %d)", rows, rowID)
	}

	push(`{"clients":[]}`, nil)
	if rows := fallbackRows(t, node.Id); len(rows) != 1 {
		t.Errorf("a push without the field changed the rows: %+v", rows)
	}

	push(`{"clients":[]}`, fallbackSet())
	if rows := fallbackRows(t, node.Id); len(rows) != 0 {
		t.Errorf("an empty pushed set left rows behind: %+v", rows)
	}
}
