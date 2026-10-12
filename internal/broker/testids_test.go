package broker

import "strings"

// Gate-marker test ids: 32 hex characters, the shape every marker store
// validates (an id doubles as the stage dir and audit file name in these
// tests).
var (
	liveID   = strings.Repeat("1", 32)
	live2ID  = strings.Repeat("2", 32)
	goneID   = strings.Repeat("3", 32)
	nodiffID = strings.Repeat("4", 32)
	ackidID  = strings.Repeat("5", 32)
	rpidID   = strings.Repeat("6", 32)
	shutidID = strings.Repeat("7", 32)
	killidID = strings.Repeat("8", 32)
	abcID    = strings.Repeat("9", 32)
	lnkID    = strings.Repeat("0", 32)
	gp1ID    = strings.Repeat("a", 32)
	gp2ID    = strings.Repeat("b", 32)
)
