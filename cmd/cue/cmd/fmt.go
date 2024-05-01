// Copyright 2018 The CUE Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"cuelang.org/go/cue/ast"
	"cuelang.org/go/cue/build"
	"cuelang.org/go/cue/errors"
	"cuelang.org/go/cue/format"
	"cuelang.org/go/cue/load"
	"cuelang.org/go/cue/token"
	"cuelang.org/go/internal/encoding"
	"cuelang.org/go/internal/source"
	"cuelang.org/go/tools/fix"

	"github.com/rogpeppe/go-internal/diff"
	"github.com/spf13/cobra"
)

func newFmtCmd(c *Command) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "fmt [-s] [inputs]",
		Short: "formats CUE configuration files",
		Long: `Fmt formats the given files or the files for the given packages in place
`,
		RunE: mkRunE(c, func(cmd *Command, args []string) error {
			plan, err := newBuildPlan(cmd, &config{loadCfg: &load.Config{
				Tests:       true,
				Tools:       true,
				AllCUEFiles: true,
				Package:     "*",
			}})
			exitOnErr(cmd, err, true)

			builds := loadFromArgs(args, plan.cfg.loadCfg)
			if builds == nil {
				exitOnErr(cmd, errors.Newf(token.NoPos, "invalid args"), true)
			}

			opts := []format.Option{}
			if flagSimplify.Bool(cmd) {
				opts = append(opts, format.Simplify())
			}

			cfg := *plan.encConfig
			cfg.Format = opts
			cfg.Force = true

			var foundBadlyFormatted bool
			check := flagCheck.Bool(cmd)
			doDiff := flagDiff.Bool(cmd)
			cwd, _ := os.Getwd()
			stdout := cmd.OutOrStdout()

			for _, inst := range builds {
				if inst.Err != nil {
					switch {
					case errors.As(inst.Err, new(*load.PackageError)) && len(inst.BuildFiles) != 0:
						// Ignore package errors if there are files to format.
					case errors.As(inst.Err, new(*load.NoFilesError)):
					default:
						exitOnErr(cmd, inst.Err, false)
						continue
					}
				}
				for _, file := range inst.BuildFiles {
					shouldFormat := inst.User || file.Filename == "-" || filepath.Dir(file.Filename) == inst.Dir
					if !shouldFormat {
						continue
					}

					// We buffer the input and output bytes to compare them.
					// This allows us to determine whether a file is already
					// formatted, without modifying the file.
					var original []byte
					var formatted bytes.Buffer
					if bs, ok := file.Source.([]byte); ok {
						original = bs
					} else {
						original, err = source.ReadAll(file.Filename, file.Source)
						exitOnErr(cmd, err, true)
						file.Source = original
					}
					cfg.Out = &formatted
					if file.Filename == "-" && !doDiff && !check {
						// Always write to stdout if the file is read from stdin.
						cfg.Out = io.MultiWriter(cfg.Out, stdout)
					}

					var files []*ast.File
					d := encoding.NewDecoder(cmd.ctx, file, &cfg)
					for ; !d.Done(); d.Next() {
						f := d.File()

						if file.Encoding == build.CUE {
							f = fix.File(f)
						}

						files = append(files, f)
					}
					// Do not defer this Close call, as we are looping over builds,
					// and otherwise we would hold all of their files open at once.
					// Note that we always call Close after NewDecoder as well.
					d.Close()
					if err := d.Err(); err != nil {
						exitOnErr(cmd, err, true)
					}

					e, err := encoding.NewEncoder(cmd.ctx, file, &cfg)
					exitOnErr(cmd, err, true)

					for _, f := range files {
						err := e.EncodeFile(f)
						exitOnErr(cmd, err, false)
					}

					if err := e.Close(); err != nil {
						exitOnErr(cmd, err, true)
					}

					// File is already well formatted; we can stop here.
					if bytes.Equal(formatted.Bytes(), original) {
						continue
					}

					foundBadlyFormatted = true
					var path string
					f := file.Filename
					path, err = filepath.Rel(cwd, f)
					if err != nil {
						path = f
					}

					switch {
					case doDiff:
						d := diff.Diff(path+".orig", original, path, formatted.Bytes())
						fmt.Fprintln(stdout, string(d))
					case check:
						fmt.Fprintln(stdout, path)
					case file.Filename == "-":
						// already written to stdout during encoding
					default:
						if err := os.WriteFile(file.Filename, formatted.Bytes(), 0644); err != nil {
							exitOnErr(cmd, err, false)
						}
					}
				}
			}

			if check && foundBadlyFormatted {
				return ErrPrintedError
			}

			return nil
		}),
	}

	cmd.Flags().Bool(string(flagCheck), false, "exits with non-zero status if any files are not formatted")
	cmd.Flags().BoolP(string(flagDiff), "d", false, "display diffs instead of rewriting files")

	return cmd
}
