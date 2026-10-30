package platform

import (
	"strconv"

	"gorm.io/gorm"
)

// Economic policies keep their legacy source rows (pricing_versions,
// gpu_price_versions, credit_policy_versions, idle_policy_versions) as internal
// foreign-key targets. The external API only ever exposes and accepts
// policy_versions.id; uq_policy_source(kind,source_id) makes both directions
// unambiguous.

// PolicyID returns the decimal policy_versions.id for a legacy source row, or nil
// when the source reference is empty.
func PolicyID(db *gorm.DB, kind string, source any) (any, error) {
	s := Uint(source)
	if s == 0 {
		return nil, nil
	}
	var ids []uint64
	if e := db.Table("policy_versions").Where("kind = ? AND source_id = ?", kind, s).Limit(1).Pluck("id", &ids).Error; e != nil {
		return nil, WrapDB(e)
	}
	if len(ids) == 0 {
		return nil, E(500, "internal_error", "policy source has no published policy version")
	}
	return strconv.FormatUint(ids[0], 10), nil
}

// PolicySource resolves an external policy_versions.id of the given kind to the
// policy row and its legacy source row id. A missing ID or an ID of another kind
// is a request validation error.
func PolicySource(db *gorm.DB, kind, id string, lock bool) (Row, uint64, error) {
	pid, e := IDString(id)
	if e != nil {
		return nil, 0, e
	}
	p, e := Get(db, "policy_versions", pid, lock)
	if e != nil {
		if he, ok := e.(*HTTPError); ok && he.Status == 404 {
			return nil, 0, E(422, "validation_failed", "policy version does not exist: "+kind)
		}
		return nil, 0, e
	}
	if Text(p["kind"]) != kind || Uint(p["source_id"]) == 0 {
		return nil, 0, E(422, "validation_failed", "policy version is not of kind "+kind)
	}
	return p, Uint(p["source_id"]), nil
}
