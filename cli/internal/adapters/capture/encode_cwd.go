package capture

// encodeCwd encodes the absolute cwd path according to the Claude session directory naming rules (CAPTURE §1.4).
//
// Rule (empirically verified from ~/.claude/projects directory names): replace every non-alphanumeric character outside [a-zA-Z0-9] with '-'. The previous implementation replaced only '/' and '.', causing capture and materialization to silently no-op when paths containing '_' or spaces mapped to different directories (audit finding #7).
func encodeCwd(absPath string) string {
	out := make([]rune, 0, len(absPath))
	for _, r := range absPath {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			out = append(out, r)
		} else {
			out = append(out, '-')
		}
	}
	return string(out)
}
