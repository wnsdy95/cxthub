package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func runRemoteRepair(ctx context.Context, c *Container, cwd string, p parsedCommand) error {
	if p.has("--preview") {
		if c.PreviewRemoteRepair == nil {
			return fmt.Errorf("remote repair preview unavailable")
		}
		plan, err := c.PreviewRemoteRepair(ctx, cwd, p.flags["--ref"], domain.ContentHash(p.flags["--snapshot"]), p.flags["--reason"])
		if err != nil {
			return err
		}
		raw, err := json.MarshalIndent(plan, "", "  ")
		if err != nil {
			return err
		}
		f, err := os.OpenFile(p.flags["--output"], os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, err = f.Write(raw)
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		fmt.Printf("repair preview %s saved to %s; no pointers changed\n", plan.ID, p.flags["--output"])
		fmt.Printf("review the file, then run cxt repair --apply <file> --expect %s\n", plan.ID)
		return nil
	}
	if c.ApplyRemoteRepair == nil {
		return fmt.Errorf("remote repair unavailable")
	}
	f, err := providerfs.OpenRegularFile(p.flags["--apply"])
	if err != nil {
		return err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 16<<20+1))
	if err != nil {
		return err
	}
	if len(raw) > 16<<20 {
		return fmt.Errorf("repair plan exceeds 16 MiB")
	}
	var plan outbound.RemoteRepairPlan
	if err := json.Unmarshal(raw, &plan); err != nil {
		return err
	}
	receipt, err := c.ApplyRemoteRepair(ctx, cwd, domain.ContentHash(p.flags["--expect"]), plan)
	if err != nil {
		return err
	}
	fmt.Printf("repair receipt %s: state=applied applied_at=%s; previous state retained\n", receipt.Plan.ID, receipt.AppliedAt.UTC().Format(time.RFC3339Nano))
	fmt.Println("Retries return the recorded receipt without reapplying or checking current remote/local state.")
	return nil
}
