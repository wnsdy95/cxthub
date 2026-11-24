package gitctx

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// originPushNote is what Git writes beside a branch of origin when a push of
// this clone moved it. A fetch, and a ref somebody set by hand, carry another
// note or none.
const originPushNote = "update by push"

// OriginPushes lists the pushes this clone made to branches of origin, the
// newest first and at most RecordedPushLimit of them. It reads the record Git
// itself keeps beside each of origin's branches and asks no remote. Git wrote
// that record when the push ended, whether or not cxt ran at that moment, so
// a push made while cxt was not working is still read here later. A clone
// that pushed nothing, or that holds no branch of origin, has none.
func OriginPushes(ctx context.Context, cwd string) ([]domain.RecordedPush, error) {
	_, branches := followedBranches(ctx, cwd)
	out, err := gitRead(ctx, cwd, nil, "log", "--walk-reflogs", "--date=unix", "--format=%H%x09%gD%x09%gs", "--glob="+branches+"*")
	if err != nil {
		return nil, err
	}
	return recordedPushes(string(out), branches), nil
}

// recordedPushes reads what Git listed, one record a line: commit, branch
// with the time of the record, note. Git since 2.14 lists the records of all
// branches newest first; an older one lists branch after branch, so the order
// is made here.
func recordedPushes(listed, branches string) []domain.RecordedPush {
	pushes := []domain.RecordedPush{}
	for _, line := range strings.Split(listed, "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 3 || fields[2] != originPushNote || !fullCommitNumber(fields[0]) {
			continue
		}
		// The branch is named with the time of the record: name@{seconds}.
		at := strings.LastIndex(fields[1], "@{")
		if at < 0 || !strings.HasSuffix(fields[1], "}") {
			continue
		}
		seconds, err := strconv.ParseInt(fields[1][at+2:len(fields[1])-1], 10, 64)
		if branch := strings.TrimPrefix(fields[1][:at], branches); err == nil && seconds > 0 && branch != "" {
			pushes = append(pushes, domain.RecordedPush{Branch: branch, SHA: fields[0], AtSec: seconds})
		}
	}
	sort.SliceStable(pushes, func(left, right int) bool { return pushes[left].AtSec > pushes[right].AtSec })
	if len(pushes) > domain.RecordedPushLimit {
		pushes = pushes[:domain.RecordedPushLimit]
	}
	return pushes
}
