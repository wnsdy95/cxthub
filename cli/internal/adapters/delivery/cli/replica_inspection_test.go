package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

type inspectionReaderForTest struct{}

func (inspectionReaderForTest) ResolveRepo(ctx context.Context, root, cwd string) (domain.Repo, error) {
	return remotecfg.Wrap(root, gitctx.NewGitContextAdapter()).CurrentRepo(ctx, cwd)
}

func (inspectionReaderForTest) InspectReplica(ctx context.Context, root string) domain.ReplicaInspection {
	return storage.NewFileStore(root).InspectReplica(ctx)
}

func (inspectionReaderForTest) ListBackfills(ctx context.Context, root, repo string) ([]domain.SnapshotBackfill, error) {
	return storage.NewFileStore(root).ListBackfills(ctx, repo)
}

type recordingReplicaInspector struct {
	inspectionReaderForTest
	root       string
	cwd        string
	inspected  int
	resolved   int
	listed     int
	incomplete bool
}

func (reader *recordingReplicaInspector) ResolveRepo(ctx context.Context, root, cwd string) (domain.Repo, error) {
	reader.root, reader.cwd = root, cwd
	reader.resolved++
	return reader.inspectionReaderForTest.ResolveRepo(ctx, root, cwd)
}

func (reader *recordingReplicaInspector) InspectReplica(ctx context.Context, root string) domain.ReplicaInspection {
	reader.root = root
	reader.inspected++
	if reader.incomplete {
		return domain.ReplicaInspection{Snapshots: 3, DocumentsChecked: 1}
	}
	return reader.inspectionReaderForTest.InspectReplica(ctx, root)
}

func (reader *recordingReplicaInspector) ListBackfills(ctx context.Context, root, repo string) ([]domain.SnapshotBackfill, error) {
	reader.root = root
	reader.listed++
	return reader.inspectionReaderForTest.ListBackfills(ctx, root, repo)
}

func TestInspectionMissingReaderDoesNotClaimHealthy(test *testing.T) {
	cwd, _, _, _, _ := historyFixture(test)
	before := treeBytes(test, cwd)
	var output bytes.Buffer
	captureCalled := false
	err := RunDiagnostics(context.Background(), nil, cwd, []string{"doctor", "--json"}, &output, func(context.Context, string) ([]domain.CaptureRecoveryStatus, error) {
		captureCalled = true
		return nil, nil
	})
	var diagnostic diagnosticsReport
	if err == nil || json.Unmarshal(output.Bytes(), &diagnostic) != nil || diagnostic.Completed || diagnostic.ReplicaInspectionCompleted || captureCalled {
		test.Fatalf("missing inspection was accepted: error=%v report=%s", err, output.String())
	}
	if !reflect.DeepEqual(diagnostic.Unchecked, []string{"replica inspection", "historical uploads", "capture recovery"}) {
		test.Fatalf("unchecked scope lost: %+v", diagnostic)
	}
	output.Reset()
	err = RunSyncStatus(context.Background(), nil, cwd, true, &output)
	var status syncStatus
	if err == nil || json.Unmarshal(output.Bytes(), &status) != nil || len(status.Issues) == 0 || status.ServerChecked || status.IntegrityChecked {
		test.Fatalf("missing status reader accepted: error=%v report=%s", err, output.String())
	}
	if !reflect.DeepEqual(before, treeBytes(test, cwd)) {
		test.Fatal("missing reader created or repaired files")
	}
}

func TestInspectionIncompleteReaderPreservesUncheckedScope(test *testing.T) {
	cwd, _, _, _, _ := historyFixture(test)
	reader := &recordingReplicaInspector{incomplete: true}
	var output bytes.Buffer
	err := RunDiagnostics(context.Background(), reader, cwd, []string{"doctor", "--json"}, &output)
	var report diagnosticsReport
	if err == nil || json.Unmarshal(output.Bytes(), &report) != nil || report.Completed || report.ReplicaInspectionCompleted || report.DocumentsChecked != 1 || report.Snapshots != 3 {
		test.Fatalf("incomplete inspection was promoted: error=%v report=%s", err, output.String())
	}
	if reader.resolved != 0 || reader.listed != 0 || !strings.Contains(strings.Join(report.Issues, ";"), "inspection incomplete") || len(report.Unchecked) != 3 {
		test.Fatalf("later phases ran after incomplete inspection: %+v %+v", reader, report)
	}
}

func TestInspectionPortsPreserveObservedRootAndMetadataOnlyStatus(test *testing.T) {
	root, _, _, _, _ := historyFixture(test)
	cwd := filepath.Join(root, "nested")
	if err := os.Mkdir(cwd, 0700); err != nil {
		test.Fatal(err)
	}
	before := treeBytes(test, root)
	observedRoot := gitctx.InspectContextRoot(context.Background(), cwd).Root
	reader := &recordingReplicaInspector{}
	var output bytes.Buffer
	if err := RunSyncStatus(context.Background(), reader, cwd, true, &output); err != nil {
		test.Fatal(err)
	}
	if reader.root != observedRoot || reader.cwd != cwd || reader.resolved != 1 || reader.listed != 1 || reader.inspected != 0 {
		test.Fatalf("status rescanned bodies or changed root: %+v", reader)
	}
	output.Reset()
	if err := RunDiagnostics(context.Background(), reader, cwd, []string{"doctor", "--json"}, &output); err != nil {
		test.Fatal(err)
	}
	if reader.root != observedRoot || reader.cwd != cwd || reader.inspected != 1 || reader.resolved != 2 || reader.listed != 2 {
		test.Fatalf("doctor changed reader ownership: %+v", reader)
	}
	if !reflect.DeepEqual(before, treeBytes(test, root)) {
		test.Fatal("inspection mutated local data")
	}
}
