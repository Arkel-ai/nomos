package types

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
)

// JSONB is a raw JSON document stored in a Postgres `jsonb` column.
//
// It exists because a plain []byte binds as bytea (which Postgres will not
// cast to jsonb) and gorm.io/datatypes is not a dependency of this repo. The
// zero value is NULL, not `'null'::jsonb`, so an unset column stays unset.
//
// Per docs/PLAN.md §9.1: jsonb is for genuinely open-ended data only — an
// extraction payload, a question's option list, an event payload. Never for a
// field that is filtered, joined or sorted on.
type JSONB json.RawMessage

// Value implements driver.Valuer. An empty document is stored as NULL.
func (j JSONB) Value() (driver.Value, error) {
	if len(j) == 0 {
		return nil, nil
	}
	return string(j), nil
}

// Scan implements sql.Scanner.
func (j *JSONB) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*j = nil
	case []byte:
		*j = append(JSONB(nil), v...)
	case string:
		*j = JSONB(v)
	default:
		return fmt.Errorf("types.JSONB: cannot scan %T", src)
	}
	return nil
}

// MarshalJSON emits the stored document verbatim so an API response nests the
// jsonb payload rather than base64-encoding it.
func (j JSONB) MarshalJSON() ([]byte, error) {
	if len(j) == 0 {
		return []byte("null"), nil
	}
	return j, nil
}

// UnmarshalJSON stores the incoming document verbatim.
func (j *JSONB) UnmarshalJSON(b []byte) error {
	*j = append(JSONB(nil), b...)
	return nil
}

// GormDataType tells the GORM migrator the column type.
func (JSONB) GormDataType() string { return "jsonb" }
