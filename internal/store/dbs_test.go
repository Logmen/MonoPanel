package store

import (
	"context"
	"errors"
	"testing"
)

func TestDatabases(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	u := &User{Login: "alex"}
	db.CreateUser(ctx, u)
	inst := &DBInstance{Engine: EnginePercona, Status: DBInstalling, Service: "mysql.service"}
	if err := db.UpsertDBInstance(ctx, inst); err != nil || inst.ID == 0 {
		t.Fatalf("instance: %v", err)
	}
	inst.Status, inst.Version = DBReady, "8.4.6"
	db.UpsertDBInstance(ctx, inst)
	got, _ := db.GetDBInstance(ctx)
	if got.ID != inst.ID || got.Version != "8.4.6" {
		t.Fatalf("instance upsert: %+v", got)
	}
	d := &Database{UserID: u.ID, Name: "alex_shop"}
	if err := db.CreateDatabase(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateDatabase(ctx, &Database{UserID: u.ID, Name: "alex_shop"}); !errors.Is(err, ErrExists) {
		t.Fatalf("dup: %v", err)
	}
	if err := db.CreateDBUser(ctx, &DBUser{UserID: u.ID, DatabaseID: &d.ID, Name: "alex_shop"}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateDBUser(ctx, &DBUser{UserID: u.ID, DatabaseID: &d.ID, Name: "alex_shop", Host: "%"}); err != nil {
		t.Fatal(err)
	}
	list, _ := db.ListDatabases(ctx, u.ID)
	if len(list) != 1 || list[0].Login != "alex" || len(list[0].Users) != 2 || list[0].Collation != "utf8mb4_0900_ai_ci" {
		t.Fatalf("list: %+v", list[0])
	}
	if err := db.DeleteDatabase(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	if users, _ := db.ListDBUsers(ctx, d.ID); len(users) != 0 {
		t.Fatal("db users must cascade")
	}
}
