package domain

// BackupReason is why a backup run failed. It is its own type rather than a
// member of Reason: the two sets answer different questions to different
// callers, and merging them would let a deploy failure code reach a backup row
// or the reverse without anything noticing (spec 0020, AC-10).
//
// The set is closed. It is what `backup_runs.failure_reason` stores, what the
// admin page renders, and the whole of both. The wrapped error behind a failure
// never reaches the row, the page, or the platform log at info level.
type BackupReason string

// The seven codes a backup run can end on.
const (
	// BackupSnapshotFailed is VACUUM INTO not producing a snapshot: the
	// statement itself failed, or the file it should have written is not there.
	BackupSnapshotFailed BackupReason = "snapshot_failed"
	// BackupIntegrityFailed is the plaintext snapshot failing PRAGMA
	// integrity_check, or holding no accounts at all. Nothing is uploaded.
	BackupIntegrityFailed BackupReason = "integrity_failed"
	// BackupEncryptFailed is age failing to write the ciphertext, or the header
	// of what it wrote not naming the configured recipient. Nothing is uploaded.
	BackupEncryptFailed BackupReason = "encrypt_failed"
	// BackupUploadFailed is the object not reaching the bucket.
	BackupUploadFailed BackupReason = "upload_failed"
	// BackupVerifyFailed is the read back disagreeing with what was written on
	// length or on SHA-256. The object is left in place, because a partly
	// readable backup is worth more than none (AC-6).
	BackupVerifyFailed BackupReason = "verify_failed"
	// BackupStranded is a row a dead predecessor left running, ended at startup.
	// One replica and a Recreate strategy make such a row definitionally dead
	// (AC-9).
	BackupStranded BackupReason = "stranded"
	// BackupInternal is any fault inside the platform: a write that does not
	// land, a full volume, a locked database. It is deliberately distinct from
	// the concurrency refusal, because reporting a real fault to an admin as a
	// benign one is how this feature would lie about its own health (AC-8a).
	BackupInternal BackupReason = "internal"
)

// Valid reports whether r is one of the closed codes.
func (r BackupReason) Valid() bool {
	switch r {
	case BackupSnapshotFailed, BackupIntegrityFailed, BackupEncryptFailed,
		BackupUploadFailed, BackupVerifyFailed, BackupStranded, BackupInternal:
		return true
	default:
		return false
	}
}

// String returns the code as it is stored and rendered.
func (r BackupReason) String() string { return string(r) }
