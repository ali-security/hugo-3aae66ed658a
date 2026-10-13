// Copyright 2018 The Hugo Authors. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build extended

package scss

import (
	"fmt"
	"io"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bep/golibsass/libsass"
	"github.com/bep/golibsass/libsass/libsasserrors"
	"github.com/gohugoio/hugo/common/herrors"
	"github.com/gohugoio/hugo/helpers"
	"github.com/gohugoio/hugo/hugofs"
	"github.com/gohugoio/hugo/identity"
	"github.com/gohugoio/hugo/media"
	"github.com/gohugoio/hugo/resources"
	"github.com/gohugoio/hugo/resources/resource_transformers/tocss/sass"
)

// Used in tests. This feature requires Hugo to be built with the extended tag.
func Supports() bool {
	return true
}

func (t *toCSSTransformation) Transform(ctx *resources.ResourceTransformationCtx) error {
	ctx.OutMediaType = media.Builtin.CSSType

	var outName string
	if t.options.from.TargetPath != "" {
		ctx.OutPath = t.options.from.TargetPath
	} else {
		ctx.ReplaceOutPathExtension(".css")
	}

	outName = path.Base(ctx.OutPath)

	options := t.options
	baseDir := path.Dir(ctx.SourcePath)
	options.to.IncludePaths = t.c.sfs.RealDirs(baseDir)

	// Append any workDir relative include paths
	for _, ip := range options.from.IncludePaths {
		info, err := t.c.workFs.Stat(filepath.Clean(ip))
		if err == nil {
			filename := info.(hugofs.FileMetaInfo).Meta().Filename
			options.to.IncludePaths = append(options.to.IncludePaths, filename)
		}
	}

	varsStylesheet := sass.CreateVarsStyleSheet(sass.TranspilerLibSass, options.from.Vars)

	// The directories of the imports LibSASS resolved on its own, keyed by the
	// relative path it passes as prev for the imports inside them.
	libSassImportDirs := make(map[string][]string)

	// To allow for overrides of SCSS files anywhere in the project/theme hierarchy, we need
	// to help libsass revolve the filename by looking in the composite filesystem first.
	// We add the entry directories for both project and themes to the include paths list, but
	// that only work for overrides on the top level.
	options.to.ImportResolver = func(url string, prev string) (newUrl string, body string, resolved bool) {
		if url == sass.HugoVarsNamespace {
			return url, varsStylesheet, true
		}

		// We get URL paths from LibSASS, but we need file paths.
		url = filepath.FromSlash(url)
		prev = filepath.FromSlash(prev)

		var (
			prevDir string
			inHugo  = true
			osDirs  = options.to.IncludePaths
		)

		if prev == "stdin" {
			prevDir = baseDir
		} else {
			prevDir, inHugo = t.c.sfs.MakePathRelative(filepath.Dir(prev), true)
			osDirs = append([]string{filepath.Dir(prev)}, osDirs...)
		}

		urlDir := filepath.Dir(url)
		name := filepath.Base(url)

		// Libsass throws an error in cases where you have several possible candidates.
		// We make this simpler and pick the first match.
		var namePatterns []string
		if strings.Contains(name, ".") {
			namePatterns = []string{"_%s", "%s"}
		} else if strings.HasPrefix(name, "_") {
			namePatterns = []string{"_%s.scss", "_%s.sass"}
		} else {
			namePatterns = []string{
				"_%s.scss", "%s.scss",
				"_%s.sass", "%s.sass",
				"%s/_index.scss", "%s/_index.sass",
				"%s/index.scss", "%s/index.sass",
			}
		}

		name = strings.TrimPrefix(name, "_")
		names := make([]string, len(namePatterns))
		for i, namePattern := range namePatterns {
			names[i] = filepath.Join(urlDir, fmt.Sprintf(namePattern, name))
		}

		if inHugo {
			for _, n := range names {
				filenameToCheck := filepath.Join(prevDir, n)
				fi, err := t.c.sfs.Fs.Stat(filenameToCheck)
				if err == nil {
					if fim, ok := fi.(hugofs.FileMetaInfo); ok {
						ctx.DependencyManager.AddIdentity(identity.CleanStringIdentity(filenameToCheck))
						return fim.Meta().Filename, "", true
					}
				}
			}
		}

		// Not found in Hugo's file systems. LibSASS would now search relative to prev
		// and the include paths on its own, so do that here where the result can be checked.
		filename, err := sass.FindFile(t.c.rs.ExecHelper, osDirs, names)
		if err != nil {
			// The resolver cannot return an error, but @error fails the build.
			return url, fmt.Sprintf("@error %q;", err.Error()), true
		}
		if filename != "" {
			return filename, "", true
		}

		// Not found. LibSASS will now search on its own, relative to the importing file
		// (the current directory for the entry file) and in the include paths, and it
		// accepts more candidates than Hugo does (the name as given and .css files).
		// Whatever it may pick must be inside the allowed paths, too.
		var baseDirs []string
		switch {
		case prev == "stdin":
			baseDirs = []string{"."}
		case filepath.IsAbs(prev):
			baseDirs = []string{filepath.Dir(prev)}
		default:
			// Imported from a file LibSASS resolved on its own.
			baseDirs = libSassImportDirs[filepath.Clean(prev)]
			if baseDirs == nil {
				baseDirs = []string{filepath.Dir(prev)}
			}
		}
		for _, dir := range append(slices.Clone(baseDirs), options.to.IncludePaths...) {
			for _, candidate := range libSassCandidates(url) {
				filename := candidate
				if !filepath.IsAbs(filename) {
					filename = filepath.Join(dir, candidate)
				}
				fi, err := hugofs.Os.Stat(filename)
				if err != nil || fi.IsDir() {
					continue
				}
				if abs, err := filepath.Abs(filename); err == nil {
					filename = abs
				}
				if err := t.c.rs.ExecHelper.CheckReadPath(filename); err != nil {
					return url, fmt.Sprintf("@error %q;", err.Error()), true
				}
				if !filepath.IsAbs(candidate) {
					key := filepath.Clean(candidate)
					libSassImportDirs[key] = append(libSassImportDirs[key], filepath.Dir(filename))
				}
			}
		}

		// Let LibSASS resolve it or report it.
		return "", "", false
	}

	if ctx.InMediaType.SubType == media.Builtin.SASSType.SubType {
		options.to.SassSyntax = true
	}

	if options.from.EnableSourceMap {

		options.to.SourceMapOptions.Filename = outName + ".map"
		options.to.SourceMapOptions.Root = t.c.rs.Cfg.BaseConfig().WorkingDir

		// Setting this to the relative input filename will get the source map
		// more correct for the main entry path (main.scss typically), but
		// it will mess up the import mappings. As a workaround, we do a replacement
		// in the source map itself (see below).
		// options.InputPath = inputPath
		options.to.SourceMapOptions.OutputPath = outName
		options.to.SourceMapOptions.Contents = true
		options.to.SourceMapOptions.OmitURL = false
		options.to.SourceMapOptions.EnableEmbedded = false
	}

	res, err := t.c.toCSS(options.to, ctx.To, ctx.From)
	if err != nil {
		if sasserr, ok := err.(libsasserrors.Error); ok {
			if sasserr.File == "stdin" && ctx.SourcePath != "" {
				sasserr.File = t.c.sfs.RealFilename(ctx.SourcePath)
				err = sasserr
			}
		}
		return herrors.NewFileErrorFromFileInErr(err, hugofs.Os, nil)

	}

	if options.from.EnableSourceMap && res.SourceMapContent != "" {
		sourcePath := t.c.sfs.RealFilename(ctx.SourcePath)

		if strings.HasPrefix(sourcePath, t.c.rs.Cfg.BaseConfig().WorkingDir) {
			sourcePath = strings.TrimPrefix(sourcePath, t.c.rs.Cfg.BaseConfig().WorkingDir+helpers.FilePathSeparator)
		}

		// This needs to be Unix-style slashes, even on Windows.
		// See https://github.com/gohugoio/hugo/issues/4968
		sourcePath = filepath.ToSlash(sourcePath)

		// This is a workaround for what looks like a bug in Libsass. But
		// getting this resolution correct in tools like Chrome Workspaces
		// is important enough to go this extra mile.
		mapContent := strings.Replace(res.SourceMapContent, `stdin"`, fmt.Sprintf("%s\"", sourcePath), 1)

		return ctx.PublishSourceMap(mapContent)
	}
	return nil
}

func (c *Client) toCSS(options libsass.Options, dst io.Writer, src io.Reader) (libsass.Result, error) {
	var res libsass.Result

	transpiler, err := libsass.New(options)
	if err != nil {
		return res, err
	}

	in := helpers.ReaderToString(src)

	// See https://github.com/gohugoio/hugo/issues/7059
	// We need to preserve the regular CSS imports. This is by far
	// a perfect solution, and only works for the main entry file, but
	// that should cover many use cases, e.g. using SCSS as a preprocessor
	// for Tailwind.
	var importsReplaced bool
	in, importsReplaced = replaceRegularImportsIn(in)

	res, err = transpiler.Execute(in)
	if err != nil {
		return res, err
	}

	out := res.CSS
	if importsReplaced {
		out = replaceRegularImportsOut(out)
	}

	_, err = io.WriteString(dst, out)

	return res, err
}

var libSassExtensions = []string{".scss", ".sass", ".css"}

// libSassCandidates returns the files LibSASS tries on its own for an import of url,
// relative to the directory searched unless url is absolute.
// See resolve_includes in LibSASS' file.cpp.
func libSassCandidates(url string) []string {
	dir, name := filepath.Split(url)
	candidates := []string{name, "_" + name}
	for _, ext := range libSassExtensions {
		candidates = append(candidates, "_"+name+ext)
	}
	for _, ext := range libSassExtensions {
		candidates = append(candidates, name+ext)
	}
	if !slices.ContainsFunc(libSassExtensions, func(ext string) bool { return strings.HasSuffix(name, ext) }) {
		for _, index := range []string{"_index", "index"} {
			for _, ext := range libSassExtensions {
				candidates = append(candidates, filepath.Join(name, index+ext))
			}
		}
	}
	for i, candidate := range candidates {
		candidates[i] = filepath.Join(dir, candidate)
	}
	return candidates
}
