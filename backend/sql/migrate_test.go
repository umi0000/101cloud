package migrations

import "testing"

func TestStatementsKeepQuotedAndCommentSemicolons(t *testing.T) {
	src := "-- ignored ;\nINSERT INTO settings(value) VALUES ('a; b\\'c'); /* block ; */\nINSERT INTO settings(value) VALUES ('it''s; valid');\nCREATE TABLE `semi;colon`(id INT); # tail ;\n"
	got, e := Statements(src)
	if e != nil {
		t.Fatal(e)
	}
	want := []string{"INSERT INTO settings(value) VALUES ('a; b\\'c')", "INSERT INTO settings(value) VALUES ('it''s; valid')", "CREATE TABLE `semi;colon`(id INT)"}
	if len(got) != len(want) {
		t.Fatalf("split quoted statement: %#v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("statement %d changed SQL: %q", i, got[i])
		}
	}
}
func TestStatementsRejectIncompleteMigration(t *testing.T) {
	for _, src := range []string{"INSERT INTO t VALUES ('unfinished)", "CREATE TABLE `unfinished(id INT)", "CREATE TABLE t(id INT); /* unfinished"} {
		if _, e := Statements(src); e == nil {
			t.Fatalf("incomplete migration accepted: %s", src)
		}
	}
}
