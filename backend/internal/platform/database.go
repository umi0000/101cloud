package platform

import "errors"

// GORM map inserts expose an auto-increment key as @id when no model schema is
// attached. Normalize it before using the new row in foreign keys or responses.
func InsertedID(row Row) (uint64, error) {
	id := Uint(row["id"])
	if id == 0 {
		id = Uint(row["@id"])
	}
	if id == 0 {
		return 0, errors.New("insert did not return an auto-increment identity")
	}
	row["id"] = id
	delete(row, "@id")
	return id, nil
}
