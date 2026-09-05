package scopedbundle

import (
	"context"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestApplyRequiresExactCurrentDigestBeforeOpeningTransaction(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	bundle := minimalBundle(t)
	if err := bundle.Seal(); err != nil {
		t.Fatal(err)
	}

	_, err = Apply(context.Background(), db, bundle, ApplyOptions{})
	if err == nil || !strings.Contains(err.Error(), "expected current") {
		t.Fatalf("missing digest error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("Apply touched the database before validating options: %v", err)
	}
}

func TestApplyRejectsTamperedBundleBeforeOpeningTransaction(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	bundle := minimalBundle(t)
	if err := bundle.Seal(); err != nil {
		t.Fatal(err)
	}
	bundle.Tables[0].Rows[0].Cells[1].Text = "tampered after seal"

	_, err = Apply(context.Background(), db, bundle, ApplyOptions{
		ExpectedCurrentSHA256: strings.Repeat("0", 64),
	})
	if err == nil || !strings.Contains(err.Error(), "verify bundle: bundle digest mismatch") {
		t.Fatalf("tampered bundle error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("Apply touched the database before verifying the bundle seal: %v", err)
	}
}

func TestMaterializeDesiredUsesCompatibleTargetShape(t *testing.T) {
	bundle := minimalBundle(t)
	if err := bundle.Seal(); err != nil {
		t.Fatal(err)
	}
	target := schemaFromBundle(bundle)
	defaultValue := "0"
	target.Tables["issues"] = append(target.Tables["issues"], Column{
		Name: "target_flag", SQLType: "tinyint", Nullable: false, Default: &defaultValue,
	})

	tables, err := materializeDesired(bundle, target)
	if err != nil {
		t.Fatal(err)
	}
	issues := tableByName(t, tables, "issues")
	if got := rowValue(t, issues, 0, "target_flag"); got != "0" {
		t.Fatalf("target default = %q", got)
	}
}
