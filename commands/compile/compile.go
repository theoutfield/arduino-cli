// This file is part of arduino-cli.
//
// Copyright 2020 ARDUINO SA (http://www.arduino.cc/)
//
// This software is released under the GNU General Public License version 3,
// which covers the main part of arduino-cli.
// The terms of this license can be found at:
// https://www.gnu.org/licenses/gpl-3.0.en.html
//
// You can be released from the requirements of the above licenses by purchasing
// a commercial license. Buying such a license is mandatory if you want to
// modify or otherwise use the software for commercial activities involving the
// Arduino software without disclosing the source code of your own applications.
// To purchase a commercial license, send an email to license@arduino.cc.

package compile

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/arduino/arduino-cli/arduino/cores/packagemanager"
	"github.com/arduino/arduino-cli/arduino/sketches"
	"github.com/arduino/arduino-cli/commands"
	"github.com/arduino/arduino-cli/configuration"
	"github.com/arduino/arduino-cli/legacy/builder"
	"github.com/arduino/arduino-cli/legacy/builder/i18n"
	"github.com/arduino/arduino-cli/legacy/builder/types"
	rpc "github.com/arduino/arduino-cli/rpc/commands"
	"github.com/arduino/arduino-cli/telemetry"
	paths "github.com/arduino/go-paths-helper"
	properties "github.com/arduino/go-properties-orderedmap"
	"github.com/pkg/errors"
	"github.com/segmentio/stats/v4"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

// Compile FIXMEDOC
func Compile(ctx context.Context, req *rpc.CompileReq, outStream, errStream io.Writer, debug bool) (r *rpc.CompileResp, e error) {

	tags := map[string]string{
		"board":           req.Board,
		"fqbn":            req.Fqbn,
		"sketchPath":      telemetry.Sanitize(req.SketchPath),
		"showProperties":  strconv.FormatBool(req.ShowProperties),
		"preprocess":      strconv.FormatBool(req.Preprocess),
		"buildProperties": strings.Join(req.BuildProperties, ","),
		"warnings":        req.Warnings,
		"verbose":         strconv.FormatBool(req.Verbose),
		"quiet":           strconv.FormatBool(req.Quiet),
		"vidPid":          req.VidPid,
		"exportFile":      telemetry.Sanitize(req.ExportFile),
		"jobs":            strconv.FormatInt(int64(req.Jobs), 10),
		"libraries":       strings.Join(req.Libraries, ","),
	}

	// Use defer func() to evaluate tags map when function returns
	// and set success flag inspecting the error named return parameter
	defer func() {
		tags["success"] = "true"
		if e != nil {
			tags["success"] = "false"
		}
		stats.Incr("compile", stats.M(tags)...)
	}()

	pm := commands.GetPackageManager(req.GetInstance().GetId())
	if pm == nil {
		return nil, errors.New("invalid instance")
	}

	logrus.Tracef("Compile %s for %s started", req.GetSketchPath(), req.GetFqbn())
	if req.GetSketchPath() == "" {
		return nil, fmt.Errorf("missing sketchPath")
	}
	sketchPath := paths.New(req.GetSketchPath())
	sketch, err := sketches.NewSketchFromPath(sketchPath)
	if err != nil {
		return nil, fmt.Errorf("opening sketch: %s", err)
	}

	boardArg := req.GetBoard()
	if boardArg == "" {
		boardArg = req.GetFqbn() // DEPRECATED: Keep Fqbn field working for old clients.
	}
	if boardArg == "" && sketch != nil && sketch.Metadata != nil {
		boardArg = sketch.Metadata.CPU.Fqbn
	}
	if boardArg == "" {
		return nil, errors.Errorf("no board provided")
	}

	fqbn, _, _, err := pm.FindBoard(boardArg)
	if err != nil {
		return nil, errors.Errorf("board '%s' not found: %s", req.GetBoard(), err)
	}

	targetPlatform := pm.FindPlatform(&packagemanager.PlatformReference{
		Package:              fqbn.Package,
		PlatformArchitecture: fqbn.PlatformArch,
	})
	if targetPlatform == nil || pm.GetInstalledPlatformRelease(targetPlatform) == nil {
		// TODO: Move this error message in `cli` module
		// errorMessage := fmt.Sprintf(
		// 	"\"%[1]s:%[2]s\" platform is not installed, please install it by running \""+
		// 		version.GetAppName()+" core install %[1]s:%[2]s\".", fqbn.Package, fqbn.PlatformArch)
		// feedback.Error(errorMessage)
		return nil, fmt.Errorf("platform not installed")
	}

	builderCtx := &types.Context{}
	builderCtx.PackageManager = pm
	builderCtx.FQBN = fqbn
	builderCtx.SketchLocation = sketch.FullPath

	// FIXME: This will be redundant when arduino-builder will be part of the cli
	builderCtx.HardwareDirs = configuration.HardwareDirectories()
	builderCtx.BuiltInToolsDirs = configuration.BundleToolsDirectories()

	builderCtx.OtherLibrariesDirs = paths.NewPathList(req.GetLibraries()...)
	builderCtx.OtherLibrariesDirs.Add(configuration.LibrariesDir())

	if req.GetBuildPath() != "" {
		builderCtx.BuildPath = paths.New(req.GetBuildPath())
		err = builderCtx.BuildPath.MkdirAll()
		if err != nil {
			return nil, fmt.Errorf("cannot create build directory: %s", err)
		}
	}

	builderCtx.Verbose = req.GetVerbose()

	// Optimize for debug
	builderCtx.OptimizeForDebug = req.GetOptimizeForDebug()

	builderCtx.CoreBuildCachePath = paths.TempDir().Join("arduino-core-cache")

	builderCtx.Jobs = int(req.GetJobs())

	builderCtx.USBVidPid = req.GetVidPid()
	builderCtx.WarningsLevel = req.GetWarnings()

	if debug {
		builderCtx.DebugLevel = 100
	} else {
		builderCtx.DebugLevel = 5
	}

	builderCtx.CustomBuildProperties = append(req.GetBuildProperties(), "build.warn_data_percentage=75")

	if req.GetBuildCachePath() != "" {
		builderCtx.BuildCachePath = paths.New(req.GetBuildCachePath())
		err = builderCtx.BuildCachePath.MkdirAll()
		if err != nil {
			return nil, fmt.Errorf("cannot create build cache directory: %s", err)
		}
	}

	// Will be deprecated.
	builderCtx.ArduinoAPIVersion = "10607"

	// Check if Arduino IDE is installed and get it's libraries location.
	dataDir := paths.New(viper.GetString("directories.Data"))
	preferencesTxt := dataDir.Join("preferences.txt")
	ideProperties, err := properties.LoadFromPath(preferencesTxt)
	if err == nil {
		lastIdeSubProperties := ideProperties.SubTree("last").SubTree("ide")
		// Preferences can contain records from previous IDE versions. Find the latest one.
		var pathVariants []string
		for k := range lastIdeSubProperties.AsMap() {
			if strings.HasSuffix(k, ".hardwarepath") {
				pathVariants = append(pathVariants, k)
			}
		}
		sort.Strings(pathVariants)
		ideHardwarePath := lastIdeSubProperties.Get(pathVariants[len(pathVariants)-1])
		ideLibrariesPath := filepath.Join(filepath.Dir(ideHardwarePath), "libraries")
		builderCtx.BuiltInLibrariesDirs = paths.NewPathList(ideLibrariesPath)
	}

	builderCtx.ExecStdout = outStream
	builderCtx.ExecStderr = errStream
	builderCtx.SetLogger(i18n.LoggerToCustomStreams{Stdout: outStream, Stderr: errStream})

	// if --preprocess or --show-properties were passed, we can stop here
	if req.GetShowProperties() {
		return &rpc.CompileResp{}, builder.RunParseHardwareAndDumpBuildProperties(builderCtx)
	} else if req.GetPreprocess() {
		return &rpc.CompileResp{}, builder.RunPreprocess(builderCtx)
	}

	// if it's a regular build, go on...
	if err := builder.RunBuilder(builderCtx); err != nil {
		return nil, err
	}

	if !req.GetDryRun() {
		// FIXME: Make a function to obtain these info...
		outputPath := paths.New(
			builderCtx.BuildProperties.ExpandPropsInString("{build.path}/{recipe.output.tmp_file}")) // "/build/path/sketch.ino.bin"
		ext := outputPath.Ext()          // ".hex" | ".bin"
		base := outputPath.Base()        // "sketch.ino.hex"
		base = base[:len(base)-len(ext)] // "sketch.ino"

		// FIXME: Make a function to produce a better name...
		// Make the filename without the FQBN configs part
		fqbn.Configs = properties.NewMap()
		fqbnSuffix := strings.Replace(fqbn.String(), ":", ".", -1)

		var exportPath *paths.Path
		var exportFile string
		if req.GetExportFile() == "" {
			if sketch.FullPath.IsDir() {
				exportPath = sketch.FullPath
			} else {
				exportPath = sketch.FullPath.Parent()
			}
			exportFile = sketch.Name + "." + fqbnSuffix // "sketch.arduino.avr.uno"
		} else {
			exportPath = paths.New(req.GetExportFile()).Parent()
			exportFile = paths.New(req.GetExportFile()).Base()
			if strings.HasSuffix(exportFile, ext) {
				exportFile = exportFile[:len(exportFile)-len(ext)]
			}
		}

		// Copy "sketch.ino.*.hex" / "sketch.ino.*.bin" artifacts to sketch directory
		srcDir, err := outputPath.Parent().ReadDir() // read "/build/path/*"
		if err != nil {
			return nil, fmt.Errorf("reading build directory: %s", err)
		}
		srcDir.FilterPrefix(base + ".")
		srcDir.FilterSuffix(ext)
		for _, srcOutput := range srcDir {
			srcFilename := srcOutput.Base()       // "sketch.ino.*.bin"
			srcFilename = srcFilename[len(base):] // ".*.bin"
			dstOutput := exportPath.Join(exportFile + srcFilename)
			logrus.WithField("from", srcOutput).WithField("to", dstOutput).Debug("copying sketch build output")
			if err = srcOutput.CopyTo(dstOutput); err != nil {
				return nil, fmt.Errorf("copying output file: %s", err)
			}
		}

		// Copy .elf file to sketch directory
		srcElf := outputPath.Parent().Join(base + ".elf")
		if srcElf.Exist() {
			dstElf := exportPath.Join(exportFile + ".elf")
			logrus.WithField("from", srcElf).WithField("to", dstElf).Debug("copying sketch build output")
			if err = srcElf.CopyTo(dstElf); err != nil {
				return nil, fmt.Errorf("copying elf file: %s", err)
			}
		}
	}

	logrus.Tracef("Compile %s for %s successful", sketch.Name, boardArg)
	return &rpc.CompileResp{}, nil
}
