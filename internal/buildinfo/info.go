package buildinfo

var Commit = "unavailable"

const Version = "dev"

func CommitSHA() *string {
	if Commit == "unavailable" {
		return nil
	}
	return &Commit
}
