/*
 * Copyright 2015 Google Inc. All Rights Reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

/*
 * This module handles all file changes and serialize accesses to the DB
 * (symbols-db.go). It is event driven. There are events for file discovery,
 * file creation, deletion, renaming, and modification. There are also events
 * for DB queries, and a timer for DB flushing.
 *
 * Everything is initialized in startFilesHandler. All the events are handled in
 * the handleFiles go routine. The file discovery is run once at daemon start up
 * and it is exected by exploreIndexDir function. Function listenRequests
 * listens for any new query and sends the requests to handleFiles for
 * processing.
 *
 * For increased parallelism, we have multiple go routines for parsing (function
 * parseFiles). By default, there will be as many parseFiles go routines as CPUs
 * available. This function will simply take a file name, call the parser, and
 * return the symbolsTUDB created by the parser (presumibly for its insertion in
 * the DB). Function handleFiles sends files to be parsed according to its needs
 * (e.g. a new file was created, a file was changed, etc). It will later get the
 * new symbolsTUDB and insert it in the DB.
 *
 *   +-----------------+
 *   | exploreIndexDir |
 *   +-----------------+
 *           |
 *           |
 *           v
 *    +-------------+               +----------------------------+
 *    | handleFiles |  <--------->  | (# cpu cores) x parseFiles |
 *    +-------------+               +----------------------------+
 *           ^
 *           |
 *           |
 *   +----------------+
 *   | listenRequests |
 *   +----------------+
 */

/*
 * NOTE: There is a potential race if a included header is removed and created
 * quickly (this could be the case for vim and its backup files). To exemplofy
 * the issue, assume a file a.c that includes a header b.h. The race goes like
 * this:
 * 1. b.h is removed and navc quickly reparse a.c but does not add it yet to the
 *    DB. This new TUDB will have b.h as a potential header, but not a real one.
 * 2. While parsing, b.h is created again and navc look for potential files
 *    including a header named b.h. However, it does not find one because in the
 *    DB, a.c still have b.h as dependency. Hence, it ignores the create event.
 * 3. The new TUDB (the one without the b.h dependency) is inserted in the DB.
 *
 * In practice I havn't seen this occurring, but it might happen. The
 * consequence is that any change to b.h will not cause navc to reparse a.c.
 * This can be easily fixed in the next navc reboot or by writing to a.c for
 * reparsing.
 */

import (
	"container/list"
	"log"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	fsnotify "gopkg.in/fsnotify.v1"
)

const validCString string = `^[^\.].*\.c$`
const validHString string = `^[^\.].*\.h$`
const flushTime int = 10

var sysInclDir = map[string]bool{
	"/usr/include/": true,
	"/usr/lib/":     true,
}

var toParseMap map[string]bool
var toParseQueue *list.List
var inFlight map[string]bool
var nIndexingThreads int
var parseFile chan string
var doneFile chan *symbolsTUDB
var foundFile, foundHeader, removeFile chan string
var flush <-chan time.Time
var newConn chan net.Conn

var wg sync.WaitGroup
var watcher *fsnotify.Watcher

var db *symbolsDB
var rh *RequestHandler

func traversePath(path string, visitDir func(string), visitC func(string), visitRest func(string)) {
	filepath.Walk(path, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			log.Println("error opening", path, "igoring", err)
			return filepath.SkipDir
		}

		// visit file
		if info.IsDir() {
			if info.Name() != "." && info.Name()[0] == '.' {
				return filepath.SkipDir
			}

			visitDir(path)
			return nil
		}
		// ignore non-C files
		validC, _ := regexp.MatchString(validCString, path)
		if validC {
			visitC(path)
		} else {
			visitRest(path)
		}

		return nil
	})
}

func queueFileToParse(filePath string) {
	if len(inFlight) < nIndexingThreads && !inFlight[filePath] {
		inFlight[filePath] = true
		parseFile <- filePath
	} else if !toParseMap[filePath] {
		toParseMap[filePath] = true
		toParseQueue.PushBack(filePath)
	}
}

func queueFilesToParse(files ...string) {
	for _, f := range files {
		queueFileToParse(f)
	}
}

func doneFileToParse(tudb *symbolsTUDB) {
	if !toParseMap[tudb.File] {
		db.InsertTUDB(tudb)
	}

	delete(inFlight, tudb.File)

	if toParseQueue.Front() == nil {
		return
	}

	filePath := toParseQueue.Front().Value.(string)
	toParseQueue.Remove(toParseQueue.Front())
	delete(toParseMap, filePath)

	inFlight[filePath] = true
	parseFile <- filePath
}

func parseIncluders(headerPath string) {
	toParse, err := db.GetIncluders(headerPath)
	if err != nil {
		log.Panic(err)
	}
	queueFilesToParse(toParse...)
}

func handleFileChange(event fsnotify.Event) {
	validC, _ := regexp.MatchString(validCString, event.Name)
	validH, _ := regexp.MatchString(validHString, event.Name)

	switch {
	case validC:
		switch {
		case event.Op&(fsnotify.Create|fsnotify.Write) != 0:
			queueFilesToParse(event.Name)
		case event.Op&(fsnotify.Remove|fsnotify.Rename) != 0:
			db.RemoveFileReferences(event.Name)
		}
	case validH:
		if event.Op&(fsnotify.Write|fsnotify.Remove|fsnotify.Rename|fsnotify.Create) != 0 {
			parseIncluders(event.Name)
		}
	}
}

func handleDirChange(event fsnotify.Event) {
	switch {
	case event.Op&(fsnotify.Create) != 0:
		// explore the new dir
		visitorDir := func(path string) {
			// add watcher to directory
			watcher.Add(path)
		}
		visitorC := func(path string) {
			// put file in channel
			queueFilesToParse(path)
		}
		visitorRest := func(path string) {
			// nothing to do
		}
		traversePath(event.Name, visitorDir, visitorC, visitorRest)
	case event.Op&(fsnotify.Remove|fsnotify.Rename) != 0:
		// remove watcher from dir
		watcher.Remove(event.Name)
	}
}

func isDirectory(path string) (bool, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return fi.IsDir(), nil
}

func handleChange(event fsnotify.Event) {

	// ignore if hidden
	if filepath.Base(event.Name)[0] == '.' {
		return
	}

	// first, we need to check if the file is a directory or not
	isDir, err := isDirectory(event.Name)
	if os.IsNotExist(err) {
		// we either removed or renamed. If not found in DB, assuming
		// dir
		isDir = !db.FileExist(event.Name)
	} else if err != nil {
		// ignoring this event
		return
	}

	if isDir {
		handleDirChange(event)
	} else {
		handleFileChange(event)
	}
}

func isSysInclDir(path string) bool {
	for incl := range sysInclDir {
		if strings.HasPrefix(path, incl) {
			return true
		}
	}

	return false
}

func parseFiles(indexDir []string) {
	wg.Add(1)
	defer wg.Done()

	pa := newParser(indexDir)

	for file := range parseFile {
		log.Println("parsing", file)
		doneFile <- pa.Parse(file)
	}
}

func handleFiles(indexDir []string) {
	wg.Add(1)
	defer wg.Done()

	// start threads to process files
	for i := 0; i < nIndexingThreads; i++ {
		go parseFiles(indexDir)
	}

	for {
		select {
		// process parsed files
		case tudb, ok := <-doneFile:
			if !ok {
				return
			}
			doneFileToParse(tudb)
			// process changes in files
		case event := <-watcher.Events:
			handleChange(event)
		case err := <-watcher.Errors:
			log.Println("watcher error: ", err)
		// process explored files
		case header := <-foundHeader:
			parseIncluders(header)
		case file := <-foundFile:
			exist, uptodate, err := db.UptodateFile(file)
			if err == nil && (!exist || !uptodate) {
				queueFilesToParse(file)
			}
		case file := <-removeFile:
			validH, _ := regexp.MatchString(validHString, file)
			if validH {
				parseIncluders(file)
			} else {
				db.RemoveFileReferences(file)
			}
		// flush frequently to disk
		case <-flush:
			db.FlushDB(time.Now().Add(-time.Duration(flushTime) * time.Second))
		// handle requests
		case conn := <-newConn:
			rh.handleRequest(conn)
		}
	}
}

func exploreIndexDir(indexDir []string) {
	wg.Add(1)
	defer wg.Done()

	// explore all the paths in indexDir and process all files
	notExplored := db.GetSetFilesInDB()
	visitorDir := func(path string) {
		// add watcher to directory
		watcher.Add(path)
	}
	visitorC := func(path string) {
		// update set of removed files
		delete(notExplored, path)
		// put file in channel
		foundFile <- path
	}
	visitorRest := func(path string) {
		if notExplored[path] {
			// update set of removed files
			delete(notExplored, path)
		}
		foundHeader <- path
	}
	for _, path := range indexDir {
		traversePath(path, visitorDir, visitorC, visitorRest)
	}

	// check files not explored by now
	for path := range notExplored {
		if isSysInclDir(path) {
			// if system include dir, visit normally
			visitorRest(path)
		} else {
			// if not, then delete
			removeFile <- path
		}
	}
}

func startFilesHandler(indexDir []string, inputIndexThreads int, dbDir string) error {
	var err error

	toParseMap = make(map[string]bool)
	toParseQueue = list.New()
	inFlight = make(map[string]bool)
	nIndexingThreads = inputIndexThreads
	parseFile = make(chan string)
	doneFile = make(chan *symbolsTUDB)
	foundFile = make(chan string)
	foundHeader = make(chan string)
	removeFile = make(chan string)
	newConn = make(chan net.Conn)
	watcher, err = fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	flush = time.Tick(time.Duration(flushTime) * time.Second)
	db = newSymbolsDB(dbDir)
	rh = newRequestHandler(db)

	go listenRequests(newConn)
	go handleFiles(indexDir)
	go exploreIndexDir(indexDir)

	return nil
}

func closeFilesHandler() {
	close(parseFile)
	close(doneFile)
	watcher.Close()

	wg.Wait()

	db.FlushDB(time.Now())
}
