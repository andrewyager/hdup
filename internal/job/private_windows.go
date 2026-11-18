package job

// checkPrivate does no check on Windows, where file access comes from ACLs.
func checkPrivate(string) error { return nil }
