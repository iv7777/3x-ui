package service

import (
	"encoding/json"
	"errors"
	"net"
	"reflect"
	"strconv"
	"strings"

	"github.com/mhsanaei/3x-ui/v3/internal/database"
	"github.com/mhsanaei/3x-ui/v3/internal/database/model"

	"gorm.io/gorm"
)

type FallbackService struct{}

// FallbackInput is the payload shape POSTed by the inbound form.
type FallbackInput struct {
	ChildId   int    `json:"childId"`
	Name      string `json:"name"`
	Alpn      string `json:"alpn"`
	Path      string `json:"path"`
	Dest      string `json:"dest"`
	Xver      int    `json:"xver"`
	SortOrder int    `json:"sortOrder"`
}

// GetByMaster returns every fallback rule attached to the master inbound.
func (s *FallbackService) GetByMaster(masterId int) ([]model.InboundFallback, error) {
	var rows []model.InboundFallback
	err := database.GetDB().
		Where("master_id = ?", masterId).
		Order("sort_order ASC, id ASC").
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// GetParentForChild finds the first fallback rule that points at childId.
// Used by client-link generation: when a child inbound is attached as a
// fallback, its client links should advertise the master's address+port
// and TLS instead of the child's loopback listen.
func (s *FallbackService) GetParentForChild(childId int) (*model.InboundFallback, error) {
	var row model.InboundFallback
	err := database.GetDB().
		Where("child_id = ?", childId).
		Order("sort_order ASC, id ASC").
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// SetByMaster replaces the master's entire fallback list atomically. On a
// node-hosted inbound it also queues the push that carries the list to the node.
func (s *FallbackService) SetByMaster(masterId int, items []FallbackInput) error {
	return database.GetDB().Transaction(func(tx *gorm.DB) error {
		if err := replaceFallbackRules(tx, masterId, items); err != nil {
			return err
		}
		var master model.Inbound
		err := tx.Select("id", "node_id", "settings").Where("id = ?", masterId).Limit(1).Find(&master).Error
		if err != nil || master.Id == 0 {
			return err
		}
		// The table is the only truth from here on; a stale settings copy would
		// resurrect a rule the operator just deleted.
		if stripped, changed := removeSettingsFallbacks(master.Settings); changed {
			if err := tx.Model(&model.Inbound{}).Where("id = ?", masterId).Update("settings", stripped).Error; err != nil {
				return err
			}
		}
		if master.NodeID != nil {
			return (&NodeService{}).MarkNodeDirtyTx(tx, *master.NodeID)
		}
		return nil
	})
}

func replaceFallbackRules(tx *gorm.DB, masterId int, items []FallbackInput) error {
	if err := tx.Where("master_id = ?", masterId).Delete(&model.InboundFallback{}).Error; err != nil {
		return err
	}
	for i, c := range items {
		childId := c.ChildId
		if childId == masterId {
			childId = 0
		}
		if childId <= 0 && strings.TrimSpace(c.Dest) == "" {
			continue
		}
		row := model.InboundFallback{
			MasterId:  masterId,
			ChildId:   childId,
			Name:      c.Name,
			Alpn:      c.Alpn,
			Path:      c.Path,
			Dest:      c.Dest,
			Xver:      c.Xver,
			SortOrder: c.SortOrder,
		}
		if row.SortOrder == 0 {
			row.SortOrder = i
		}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
	}
	return nil
}

// removeSettingsFallbacks drops the settings.fallbacks key from an inbound's
// settings JSON; unparseable settings are returned untouched.
func removeSettingsFallbacks(settings string) (string, bool) {
	var parsed map[string]any
	if err := json.Unmarshal([]byte(settings), &parsed); err != nil {
		return settings, false
	}
	if _, has := parsed["fallbacks"]; !has {
		return settings, false
	}
	delete(parsed, "fallbacks")
	out, err := json.MarshalIndent(parsed, "", "  ")
	if err != nil {
		return settings, false
	}
	return string(out), true
}

// WireByMaster resolves the master's rules into the form panels exchange.
func (s *FallbackService) WireByMaster(tx *gorm.DB, masterId int) (model.FallbackSet, error) {
	byMaster, err := s.WireByMasters(tx, []int{masterId})
	if err != nil {
		return nil, err
	}
	return byMaster[masterId], nil
}

// WireByMasters resolves many masters' rules with two queries. Every requested
// master is present in the result, with an empty set when it has no rules.
func (s *FallbackService) WireByMasters(tx *gorm.DB, masterIds []int) (map[int]model.FallbackSet, error) {
	if tx == nil {
		tx = database.GetDB()
	}
	out := make(map[int]model.FallbackSet, len(masterIds))
	for _, id := range masterIds {
		out[id] = model.FallbackSet{}
	}
	if len(masterIds) == 0 {
		return out, nil
	}
	var rows []model.InboundFallback
	if err := tx.Where("master_id IN ?", masterIds).
		Order("sort_order ASC, id ASC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return out, nil
	}

	childIds := make([]int, 0, len(rows))
	for i := range rows {
		childIds = append(childIds, rows[i].ChildId)
	}
	var children []model.Inbound
	if err := tx.Select("id", "listen", "port").Where("id IN ?", childIds).Find(&children).Error; err != nil {
		return nil, err
	}
	byId := make(map[int]*model.Inbound, len(children))
	for i := range children {
		byId[children[i].Id] = &children[i]
	}

	for _, r := range rows {
		dest := strings.TrimSpace(r.Dest)
		if dest == "" {
			child, ok := byId[r.ChildId]
			if !ok {
				continue
			}
			dest = net.JoinHostPort(fallbackChildHost(child.Listen), strconv.Itoa(child.Port))
		}
		w := model.FallbackWire{Dest: dest, Name: r.Name, Alpn: r.Alpn, Path: r.Path}
		if r.Xver > 0 {
			w.Xver = r.Xver
		}
		out[r.MasterId] = append(out[r.MasterId], w)
	}
	return out, nil
}

// fallbackChildHost is the address Xray reaches a child inbound on from the
// same host: wildcard listens become loopback.
func fallbackChildHost(listen string) string {
	listen = strings.TrimSpace(listen)
	if listen == "" || listen == "0.0.0.0" || listen == "::" || listen == "::0" {
		return "127.0.0.1"
	}
	return listen
}

// ApplyWire makes the master's rules equal set and reports whether they changed.
// A destination that is a sibling inbound is stored as that link, keeping its client links.
func (s *FallbackService) ApplyWire(tx *gorm.DB, master *model.Inbound, set model.FallbackSet) (bool, error) {
	current, err := s.WireByMaster(tx, master.Id)
	if err != nil {
		return false, err
	}
	if len(current) == 0 && len(set) == 0 || reflect.DeepEqual(current, set) {
		return false, nil
	}
	items := make([]FallbackInput, 0, len(set))
	for i, w := range set {
		item := FallbackInput{Name: w.Name, Alpn: w.Alpn, Path: w.Path, Xver: w.Xver, SortOrder: i}
		if childId, err := siblingInboundForDest(tx, master, w.Dest); err != nil {
			return false, err
		} else if childId > 0 {
			item.ChildId = childId
		} else {
			item.Dest = w.Dest
		}
		items = append(items, item)
	}
	if err := replaceFallbackRules(tx, master.Id, items); err != nil {
		return false, err
	}
	return true, nil
}

// siblingInboundForDest finds the single inbound beside master (same node, or
// both local) that listens where dest points; 0 when none or ambiguous.
func siblingInboundForDest(tx *gorm.DB, master *model.Inbound, dest string) (int, error) {
	host, portText, err := net.SplitHostPort(strings.TrimSpace(dest))
	if err != nil {
		host, portText = "127.0.0.1", strings.TrimSpace(dest)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 {
		return 0, nil
	}
	q := tx.Model(&model.Inbound{}).Select("id", "listen").Where("port = ? AND id <> ?", port, master.Id)
	if master.NodeID == nil {
		q = q.Where("node_id IS NULL")
	} else {
		q = q.Where("node_id = ?", *master.NodeID)
	}
	var candidates []model.Inbound
	if err := q.Find(&candidates).Error; err != nil {
		return 0, err
	}
	match := 0
	for _, c := range candidates {
		if fallbackChildHost(c.Listen) != host {
			continue
		}
		if match != 0 {
			return 0, nil
		}
		match = c.Id
	}
	return match, nil
}

func (s *FallbackService) BuildFallbacksJSON(tx *gorm.DB, masterId int) ([]map[string]any, error) {
	set, err := s.WireByMaster(tx, masterId)
	if err != nil {
		return nil, err
	}
	if len(set) == 0 {
		return nil, nil
	}
	return fallbackEntries(set), nil
}

// fallbackEntries renders a rule set as the settings.fallbacks array Xray reads.
func fallbackEntries(set model.FallbackSet) []map[string]any {
	out := make([]map[string]any, 0, len(set))
	for _, w := range set {
		entry := map[string]any{"dest": w.Dest}
		if w.Name != "" {
			entry["name"] = w.Name
		}
		if w.Alpn != "" {
			entry["alpn"] = w.Alpn
		}
		if w.Path != "" {
			entry["path"] = w.Path
		}
		if w.Xver > 0 {
			entry["xver"] = w.Xver
		}
		out = append(out, entry)
	}
	return out
}
