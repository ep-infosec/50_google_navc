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

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-clang/v3.6/clang"
)

type parse struct {
	cas map[string][]string
}

/*
 * There is so much path manipulation in the construction of the compilation
 * aguments database that I think this deserves a long explanation. Compilation
 * database (compile_command.json) provides absolute path of the file with its
 * compilation options. We are storing this compilation options/arguments in
 * the cas field of the Parser struct to be used during parsing. This is a map
 * of file name to list of arguments. The name file should match the one
 * returned by the directory traversing in main, i.e., the minimum relative
 * path of the file (the path returned by filepath.Clean) or the absolute path
 * depending on the input. For each input directory (provided in the command
 * line) we try to read the compile command database from disk. For each of the
 * file path read, we fix the full path to match the relative or absolute path
 * of the input (fixPaths) and clean it with filepath.Clean.
 *
 * Then, we need to make sure that the directories in the -I options also match
 * the relative or absolute path from the input. This is fixed in fixCompDirArg
 * right before populating the arguments for some specific file.
 */

type compArgs struct {
	Directory string
	Command   string
	File      string
}

func fixPaths(cas []compArgs, path string) {
	// first, find absolute path of @path
	if filepath.IsAbs(path) {
		return
	}

	wd, err := os.Getwd()
	if err != nil {
		log.Panic("unable to get working directoy: ", err)
	}

	// second, replace absolute path with relative path and clean
	for i := range cas {
		ca := &cas[i]
		rel, err := filepath.Rel(wd, ca.File)
		if err != nil {
			log.Panic("unable to get relative path: ", err)
		}
		ca.File = filepath.Clean(rel)
	}
}

func fixCompDirArg(argDir, path string) string {
	if filepath.IsAbs(path) {
		if filepath.IsAbs(argDir) {
			return argDir
		}

		abs, err := filepath.Abs(argDir)
		if err != nil {
			log.Panic("unable to get absolute path: ",
				err)
		}
		return filepath.Clean(abs)
	}
	if filepath.IsAbs(argDir) {
		wd, err := os.Getwd()
		if err != nil {
			log.Panic("unable to get working directoy: ",
				err)
		}
		rel, err := filepath.Rel(wd, argDir)
		if err != nil {
			log.Panic("unable to get relative path: ",
				err)
		}
		return filepath.Clean(rel)
	}
	return filepath.Clean(path + "/" + argDir)
}

func getCompArgs(command, path string) []string {
	args := []string{}

	argsList := strings.Fields(command)

	for i, arg := range argsList {
		switch {
		case arg == "-D":
			args = append(args, arg, argsList[i+1])
		case strings.HasPrefix(arg, "-D"):
			args = append(args, arg)
		case arg == "-I":
			argDir := fixCompDirArg(argsList[i+1], path)
			args = append(args, "-I", argDir)
		case strings.HasPrefix(arg, "-I"):
			argDir := fixCompDirArg(
				strings.Replace(arg, "-I", "", 1),
				path)
			args = append(args, "-I", argDir)
		}
	}

	return args
}

func newParser(inputDirs []string) *parse {
	ret := &parse{make(map[string][]string)}

	// read compilation args db and fix files paths
	for _, path := range inputDirs {
		f, err := os.Open(path + "/compile_commands.json")
		if os.IsPermission(err) {
			log.Panic("error opening compile db: ", err)
		} else if err != nil {
			continue
		}
		defer f.Close()

		dec := json.NewDecoder(f)
		var cas []compArgs
		err = dec.Decode(&cas)
		if err != nil {
			log.Panic(err)
		}

		fixPaths(cas, path)

		// index compArgs by file names
		for _, ca := range cas {
			ret.cas[ca.File] = getCompArgs(ca.Command, path)
		}
	}

	return ret
}

func getSymbolFromCursor(cursor *clang.Cursor) *symbolInfo {
	if cursor.IsNull() {
		return nil
	}

	f, line, col, _ := cursor.Location().FileLocation()
	fName := filepath.Clean(f.Name())
	return &symbolInfo{
		name: cursor.Spelling(),
		usr:  cursor.USR(),
		loc: SymbolLocReq{
			fName,
			int(line),
			int(col),
		},
	}
}

func (pa *parse) Parse(file string) *symbolsTUDB {
	idx := clang.NewIndex(0, 0)
	defer idx.Dispose()

	args, ok := pa.cas[file]
	if !ok {
		args = []string{}
	}
	tu := idx.ParseTranslationUnit(file, args, nil, clang.TranslationUnit_DetailedPreprocessingRecord)
	defer tu.Dispose()

	db := newSymbolsTUDB(file, tu.File(file).Time())
	defer db.TempSaveDB()

	visitNode := func(cursor, parent clang.Cursor) clang.ChildVisitResult {
		if cursor.IsNull() {
			return clang.ChildVisit_Continue
		}

		cur := getSymbolFromCursor(&cursor)
		curFile := cur.loc.File

		if curFile == "" || curFile == "." {
			// ignore system code
			return clang.ChildVisit_Continue
		}

		// TODO: erase! this is not required
		if false {
			log.Printf("%s: %s (%s)\n",
				cursor.Kind().Spelling(),
				cursor.Spelling(),
				cursor.USR())
			log.Println(curFile, ":", cur.loc.Line, cur.loc.Col)
		}
		////////////////////////////////////
		switch cursor.Kind() {
		case clang.Cursor_FunctionDecl, clang.Cursor_StructDecl, clang.Cursor_FieldDecl,
			clang.Cursor_TypedefDecl, clang.Cursor_EnumDecl, clang.Cursor_EnumConstantDecl:
			defCursor := cursor.Definition()
			if !defCursor.IsNull() {
				def := getSymbolFromCursor(&defCursor)
				db.InsertSymbolDeclWithDef(cur, def)
			} else {
				db.InsertSymbolDecl(cur)
			}
		case clang.Cursor_MacroDefinition:
			db.InsertSymbolDeclWithDef(cur, cur)
		case clang.Cursor_VarDecl:
			db.InsertSymbolDecl(cur)
		case clang.Cursor_ParmDecl:
			if cursor.Spelling() != "" {
				db.InsertSymbolDecl(cur)
			}
		case clang.Cursor_CallExpr:
			decCursor := cursor.Referenced()
			dec := getSymbolFromCursor(&decCursor)
			db.InsertSymbolUse(cur, dec, true)
		case clang.Cursor_DeclRefExpr, clang.Cursor_TypeRef, clang.Cursor_MemberRefExpr,
			clang.Cursor_MacroExpansion:
			decCursor := cursor.Referenced()
			dec := getSymbolFromCursor(&decCursor)
			db.InsertSymbolUse(cur, dec, false)
		case clang.Cursor_InclusionDirective:
			incFile := cursor.IncludedFile()
			db.InsertHeader(cursor.Spelling(), incFile)
		}

		return clang.ChildVisit_Recurse
	}

	tu.TranslationUnitCursor().Visit(visitNode)

	return db
}
