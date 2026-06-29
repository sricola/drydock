package webui

import "syscall"

// syscallNoFollow makes os.OpenFile refuse to traverse a final-component
// symlink, so a planted <id>.diff -> /etc/passwd can't be read out of the
// audit dir. drydock runs on macOS/Linux only.
const syscallNoFollow = syscall.O_NOFOLLOW
