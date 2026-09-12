package proof

import (
	"encoding/json"
	"testing"
)

func TestRowsPreservedAllowsNewRowsButRejectsChangedHistory(t *testing.T) {
	row := func(id int, value string) map[string]Value {
		return map[string]Value{"id": {Present: true, Data: json.RawMessage([]byte(string(rune('0' + id))))}, "value": {Present: true, Data: json.RawMessage(`"` + value + `"`)}}
	}
	before := []map[string]Value{row(1, "kept"), row(2, "kept")}
	if !rowsPreserved(before, []map[string]Value{row(2, "kept"), row(3, "new"), row(1, "kept")}) {
		t.Fatal("new row made preserved history fail")
	}
	if rowsPreserved(before, []map[string]Value{row(1, "changed"), row(2, "kept")}) {
		t.Fatal("changed existing row was accepted")
	}
	if rowsPreserved(before, []map[string]Value{row(2, "kept")}) {
		t.Fatal("deleted existing row was accepted")
	}
}
