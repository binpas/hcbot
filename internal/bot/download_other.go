//go:build !unix

package bot

import "os/exec"

// killGroupOnCancel does nothing here: the context stops only yt-dlp.
func killGroupOnCancel(*exec.Cmd) {}
