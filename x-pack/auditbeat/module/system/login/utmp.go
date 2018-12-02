// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

// +build linux

package login

// #include <utmp.h>
// #include <stdlib.h>
import "C"

import (
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"syscall"
	"time"
	"unsafe"

	"github.com/pkg/errors"

	"github.com/elastic/beats/auditbeat/datastore"
	"github.com/elastic/beats/libbeat/logp"
)

const (
	bucketKeyFileRecords   = "file_records"
	bucketKeyLoginSessions = "login_sessions"
)

type Inode uint64

// FileRecord represents a UTMP file at a point in time.
type FileRecord struct {
	Inode    Inode
	Size     int64
	LastUtmp Utmp
}

// Utmp contains data from the C utmp struct.
type Utmp struct {
	UtType   int
	UtPid    int
	UtLine   string
	UtUser   string
	UtHost   string
	UtTv     time.Time
	UtAddrV6 [4]uint32
}

func newUtmp(utmpC *C.struct_utmp) Utmp {
	// See utmp(5) for the utmp struct fields.
	return Utmp{
		UtType:   int(utmpC.ut_type),
		UtPid:    int(utmpC.ut_pid),
		UtLine:   C.GoString(&utmpC.ut_line[0]),
		UtUser:   C.GoString(&utmpC.ut_user[0]),
		UtHost:   C.GoString(&utmpC.ut_host[0]),
		UtTv:     time.Unix(int64(utmpC.ut_tv.tv_sec), int64(utmpC.ut_tv.tv_usec)*1000),
		UtAddrV6: [4]uint32{uint32(utmpC.ut_addr_v6[0]), uint32(utmpC.ut_addr_v6[1]), uint32(utmpC.ut_addr_v6[2]), uint32(utmpC.ut_addr_v6[3])},
	}
}

// UtmpFileReader can read a UTMP formatted file (usually /var/log/wtmp).
type UtmpFileReader struct {
	log           *logp.Logger
	bucket        datastore.Bucket
	filePattern   string
	fileRecords   map[Inode]FileRecord
	loginSessions map[string]LoginRecord
}

// NewUtmpFileReader creates and initializes a new UTMP file reader.
func NewUtmpFileReader(log *logp.Logger, bucket datastore.Bucket, filePattern string) (*UtmpFileReader, error) {
	r := &UtmpFileReader{
		log:           log,
		bucket:        bucket,
		filePattern:   filePattern,
		fileRecords:   make(map[Inode]FileRecord),
		loginSessions: make(map[string]LoginRecord),
	}

	// Load state (file records, tty mapping) from disk
	err := r.restoreStateFromDisk()
	if err != nil {
		return nil, errors.Wrap(err, "failed to restore state from disk")
	}

	return r, nil
}

// Close performs any cleanup tasks when the UTMP reader is done.
func (r *UtmpFileReader) Close() error {
	err := r.bucket.Close()
	if err != nil {
		return errors.Wrap(err, "error closing bucket")
	}

	return nil
}

// ReadNew returns any new UTMP entries in any of the configured UTMP formatted files (usually /var/log/wtmp).
func (r *UtmpFileReader) ReadNew() ([]LoginRecord, error) {
	fileInfos, err := r.fileInfos()
	defer r.deleteOldRecords(fileInfos)

	var loginRecords []LoginRecord
	for path, fileInfo := range fileInfos {
		inode := Inode(fileInfo.Sys().(*syscall.Stat_t).Ino)

		fileRecord, isKnownFile := r.fileRecords[inode]
		var oldSize int64 = 0
		if isKnownFile {
			oldSize = fileRecord.Size
		}
		newSize := fileInfo.Size()

		if newSize < oldSize {
			// UTMP files are append-only and so this is weird. It might be a sign of
			// a highly unlikely inode reuse - or of something more nefarious.
			// Setting isKnownFile to false se we read the whole file from the beginning.
			isKnownFile = false

			r.log.Warnf("Unexpectedly, the file with inode %v (path=%v) is smaller than before - reading whole file.",
				inode, path)
		}

		if !isKnownFile || newSize != oldSize {
			r.log.Debugf("Reading file %v (inode=%v, oldSize=%v, newSize=%v)", path, inode, oldSize, newSize)

			var utmpRecords []Utmp

			// Once we start reading a file, we update the file record even if something fails -
			// otherwise we will just keep trying to re-read very frequently forever.
			defer r.updateFileRecord(inode, newSize, &utmpRecords)

			if isKnownFile {
				utmpRecords, err = r.readAfter(path, &fileRecord.LastUtmp)
			} else {
				utmpRecords, err = r.readAfter(path, nil)
			}

			if err != nil {
				return nil, errors.Wrapf(err, "error reading file %v", path)
			} else if len(utmpRecords) == 0 {
				return nil, fmt.Errorf("unexpectedly, there are no new records in file %v", path)
			} else {
				for _, utmp := range utmpRecords {
					loginRecord := r.processLoginRecord(utmp)
					if loginRecord != nil {
						loginRecord.Origin = path
						loginRecords = append(loginRecords, *loginRecord)
					}
				}
			}
		}
	}

	return loginRecords, nil
}

// deleteOldRecords clean up old file records where the inode no longer exists.
func (r *UtmpFileReader) deleteOldRecords(fileInfos map[string]os.FileInfo) {
	for savedInode, _ := range r.fileRecords {
		found := false
		for _, fileInfo := range fileInfos {
			inode := Inode(fileInfo.Sys().(*syscall.Stat_t).Ino)
			if inode == savedInode {
				found = true
				break
			}
		}

		if !found {
			r.log.Debugf("Deleting file record for old inode %d", savedInode)
			delete(r.fileRecords, savedInode)
		}
	}
}

func (r *UtmpFileReader) fileInfos() (map[string]os.FileInfo, error) {
	paths, err := filepath.Glob(r.filePattern)
	if err != nil {
		return nil, errors.Wrap(err, "failed to expand file pattern")
	}

	// Sort paths in reverse order (oldest/most-rotated file first)
	sort.Sort(sort.Reverse(sort.StringSlice(paths)))

	fileInfos := make(map[string]os.FileInfo, len(r.fileRecords))
	for _, path := range paths {
		fileInfo, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				// Skip - file might have been rotated out
				r.log.Debugf("File %v does not exist anymore.", path)
				continue
			} else {
				return nil, errors.Wrapf(err, "unexpected error when reading file %v", path)
			}
		} else if fileInfo.Sys() == nil {
			return nil, fmt.Errorf("empty stat result for file %v", path)
		}

		fileInfos[path] = fileInfo
	}

	return fileInfos, nil
}

func (r *UtmpFileReader) updateFileRecord(inode Inode, size int64, utmpRecords *[]Utmp) {
	newFileRecord := FileRecord{
		Inode: inode,
		Size:  size,
	}

	if len(*utmpRecords) > 0 {
		newFileRecord.LastUtmp = (*utmpRecords)[len(*utmpRecords)-1]
	} else {
		oldFileRecord, found := r.fileRecords[inode]
		if found {
			newFileRecord.LastUtmp = oldFileRecord.LastUtmp
		}
	}

	r.fileRecords[inode] = newFileRecord
}

// ReadAfter reads a UTMP formatted file (usually /var/log/wtmp*)
// and returns the records after the provided last known record.
// If record is nil, it returns all records in the file.
func (r *UtmpFileReader) readAfter(path string, lastKnownRecord *Utmp) ([]Utmp, error) {
	cs := C.CString(path)
	defer C.free(unsafe.Pointer(cs))

	success, err := C.utmpname(cs)
	if err != nil {
		return nil, errors.Wrap(err, "error selecting UTMP file")
	}
	if success != 0 {
		return nil, errors.New("selecting UTMP file failed")
	}

	C.setutent()
	defer C.endutent()

	reachedNewRecords := (lastKnownRecord == nil)
	var utmpRecords []Utmp
	for {
		utmpC, err := C.getutent()
		if err != nil {
			return nil, errors.Wrap(err, "error getting entry in UTMP file")
		}

		if utmpC != nil {
			utmp := newUtmp(utmpC)

			if reachedNewRecords {
				r.log.Debugf("utmp: (ut_type=%d, ut_pid=%d, ut_line=%v, ut_user=%v, ut_host=%v, ut_tv.tv_sec=%v, ut_addr_v6=%v)",
					utmp.UtType, utmp.UtPid, utmp.UtLine, utmp.UtUser, utmp.UtHost, utmp.UtTv, utmp.UtAddrV6)

				utmpRecords = append(utmpRecords, utmp)
			}

			if lastKnownRecord != nil && reflect.DeepEqual(utmp, *lastKnownRecord) {
				reachedNewRecords = true
			}
		} else {
			// Eventually, we have read all UTMP records in the file.

			if !reachedNewRecords && lastKnownRecord != nil {
				// For some reason, this file did not contain the saved record.
				// This might be a sign of a highly unlikely inode reuse -
				// or of something more nefarious. We go back to the beginning and
				// read the whole file this time.
				r.log.Warnf("Unexpectedly, the file %v did not contain the saved login record %v - reading whole file.",
					path, lastKnownRecord)

				return r.readAfter(path, nil)
			}

			break
		}
	}

	return utmpRecords, nil
}

// processLoginRecord receives UTMP login records in order and returns a LoginRecord
// where appropriate.
func (r *UtmpFileReader) processLoginRecord(utmp Utmp) *LoginRecord {
	record := LoginRecord{
		Utmp:      utmp,
		Timestamp: utmp.UtTv,
		UID:       -1,
		PID:       -1,
	}

	if utmp.UtLine != "~" {
		record.TTY = utmp.UtLine
	}

	switch utmp.UtType {
	// See utmp(5) for C constants.
	case C.RUN_LVL: // 1
		// The runlevel - though a number - is stored as
		// the ASCII character of that number.
		runlevel := string(rune(utmp.UtPid))

		// 0 - halt; 6 - reboot
		if utmp.UtUser == "shutdown" || runlevel == "0" || runlevel == "6" {
			record.Type = LoginRecordTypeShutdown

			// Clear any old logins
			// TODO: Issue logout events for login events that are still around
			// at this point.
			r.loginSessions = make(map[string]LoginRecord)
		} else {
			// Ignore runlevel changes that are not shutdown or reboot.
			return nil
		}
	case C.BOOT_TIME: // 2
		if utmp.UtLine == "~" && utmp.UtUser == "reboot" {
			record.Type = LoginRecordTypeBoot

			// Clear any old logins
			// TODO: Issue logout events for login events that are still around
			// at this point.
			r.loginSessions = make(map[string]LoginRecord)
		} else {
			record.Type = LoginRecordTypeUnknown
		}
	case C.USER_PROCESS: // 7
		record.Type = LoginRecordTypeUserLogin

		record.Username = utmp.UtUser
		record.UID = lookupUsername(record.Username)
		record.PID = utmp.UtPid
		record.IP = newIP(utmp.UtAddrV6)
		record.Hostname = utmp.UtHost

		// Store TTY from user login record for enrichment when user logout
		// record comes along (which, alas, does not contain the username).
		r.loginSessions[record.TTY] = record
	case C.DEAD_PROCESS: // 8
		savedRecord, found := r.loginSessions[record.TTY]
		if found {
			record.Type = LoginRecordTypeUserLogout
			record.Username = savedRecord.Username
			record.UID = savedRecord.UID
			record.PID = savedRecord.PID
			record.IP = savedRecord.IP
			record.Hostname = savedRecord.Hostname
		} else {
			// Skip - this is usually the DEAD_PROCESS event for
			// a previous INIT_PROCESS or LOGIN_PROCESS event -
			// those are ignored - (see default case below).
			return nil
		}
	default:
		/*
			Every other record type is ignored:
			- EMPTY - empty record
			- NEW_TIME and OLD_TIME - could be useful, but not written when time changes,
			  at least not using `date`
			- INIT_PROCESS and LOGIN_PROCESS - written on boot but do not contain any
			  interesting information
			- ACCOUNTING - not implemented according to manpage
		*/
		return nil
	}

	return &record
}

// lookupUsername looks up a username and returns its UID.
// It does not pass through errors (e.g. when the user is not found)
// but will return -1 instead.
func lookupUsername(username string) int {
	if username != "" {
		user, err := user.Lookup(username)
		if err == nil {
			uid, err := strconv.Atoi(user.Uid)
			if err == nil {
				return uid
			}
		}
	}

	return -1
}

func newIP(utAddrV6 [4]uint32) *net.IP {
	var ip net.IP

	// See utmp(5) for the utmp struct fields.
	if utAddrV6[1] != 0 || utAddrV6[2] != 0 || utAddrV6[3] != 0 {
		// IPv6
		b := make([]byte, 16)
		binary.LittleEndian.PutUint32(b[:4], utAddrV6[0])
		binary.LittleEndian.PutUint32(b[4:8], utAddrV6[1])
		binary.LittleEndian.PutUint32(b[8:12], utAddrV6[2])
		binary.LittleEndian.PutUint32(b[12:], utAddrV6[3])
		ip = net.IP(b)
	} else {
		// IPv4
		b := make([]byte, 4)
		binary.LittleEndian.PutUint32(b, utAddrV6[0])
		ip = net.IP(b)
	}

	return &ip
}

func (r *UtmpFileReader) saveStateToDisk() error {
	err := r.saveFileRecordsToDisk()
	if err != nil {
		return err
	}

	err = r.saveTTYLookupToDisk()
	if err != nil {
		return err
	}

	return nil
}

func (r *UtmpFileReader) saveFileRecordsToDisk() error {
	var buf bytes.Buffer
	encoder := gob.NewEncoder(&buf)

	for _, fileRecord := range r.fileRecords {
		err := encoder.Encode(fileRecord)
		if err != nil {
			return errors.Wrap(err, "error encoding file record")
		}
	}

	err := r.bucket.Store(bucketKeyFileRecords, buf.Bytes())
	if err != nil {
		return errors.Wrap(err, "error writing file records to disk")
	}

	r.log.Debugf("Wrote %d file records to disk", len(r.fileRecords))
	return nil
}

func (r *UtmpFileReader) saveTTYLookupToDisk() error {
	var buf bytes.Buffer
	encoder := gob.NewEncoder(&buf)

	for _, loginRecord := range r.loginSessions {
		err := encoder.Encode(loginRecord)
		if err != nil {
			return errors.Wrap(err, "error encoding login record")
		}
	}

	err := r.bucket.Store(bucketKeyLoginSessions, buf.Bytes())
	if err != nil {
		return errors.Wrap(err, "error writing login records to disk")
	}

	r.log.Debugf("Wrote %d open login sessions to disk", len(r.loginSessions))
	return nil
}

func (r *UtmpFileReader) restoreStateFromDisk() error {
	err := r.restoreFileRecordsFromDisk()
	if err != nil {
		return err
	}

	err = r.restoreTTYLookupFromDisk()
	if err != nil {
		return err
	}

	return nil
}

func (r *UtmpFileReader) restoreFileRecordsFromDisk() error {
	var decoder *gob.Decoder
	err := r.bucket.Load(bucketKeyFileRecords, func(blob []byte) error {
		if len(blob) > 0 {
			buf := bytes.NewBuffer(blob)
			decoder = gob.NewDecoder(buf)
		}
		return nil
	})
	if err != nil {
		return err
	}

	if decoder != nil {
		for {
			fileRecord := new(FileRecord)
			err = decoder.Decode(fileRecord)
			if err == nil {
				r.fileRecords[fileRecord.Inode] = *fileRecord
			} else if err == io.EOF {
				// Read all
				break
			} else {
				return errors.Wrap(err, "error decoding file record")
			}
		}
	}
	r.log.Debugf("Restored %d file records from disk", len(r.fileRecords))

	return nil
}

func (r *UtmpFileReader) restoreTTYLookupFromDisk() error {
	var decoder *gob.Decoder
	err := r.bucket.Load(bucketKeyLoginSessions, func(blob []byte) error {
		if len(blob) > 0 {
			buf := bytes.NewBuffer(blob)
			decoder = gob.NewDecoder(buf)
		}
		return nil
	})
	if err != nil {
		return err
	}

	if decoder != nil {
		for {
			loginRecord := new(LoginRecord)
			err = decoder.Decode(loginRecord)
			if err == nil {
				r.loginSessions[loginRecord.TTY] = *loginRecord
			} else if err == io.EOF {
				// Read all
				break
			} else {
				return errors.Wrap(err, "error decoding login record")
			}
		}
	}
	r.log.Debugf("Restored %d open login sessions from disk", len(r.loginSessions))

	return nil
}
