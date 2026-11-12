package tools

// Test seams for the race and crash windows of the commit.
func SetHooks(afterLoad, beforeRename, afterRename func()) (restore func()) {
	a, b, c := testAfterLoad, testBeforeRename, testAfterRename
	testAfterLoad, testBeforeRename, testAfterRename = afterLoad, beforeRename, afterRename
	return func() { testAfterLoad, testBeforeRename, testAfterRename = a, b, c }
}

func GitName(s string) bool { return gitName(s) }
