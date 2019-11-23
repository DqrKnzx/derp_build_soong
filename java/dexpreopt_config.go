// Copyright 2019 Google Inc. All rights reserved.
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

package java

import (
	"fmt"
	"path/filepath"
	"strings"

	"android/soong/android"
	"android/soong/dexpreopt"
)

// dexpreoptGlobalConfig returns the global dexpreopt.config.  It is loaded once the first time it is called for any
// ctx.Config(), and returns the same data for all future calls with the same ctx.Config().  A value can be inserted
// for tests using setDexpreoptTestGlobalConfig.
func dexpreoptGlobalConfig(ctx android.PathContext) dexpreopt.GlobalConfig {
	return dexpreoptGlobalConfigRaw(ctx).global
}

type globalConfigAndRaw struct {
	global dexpreopt.GlobalConfig
	data   []byte
}

func dexpreoptGlobalConfigRaw(ctx android.PathContext) globalConfigAndRaw {
	return ctx.Config().Once(dexpreoptGlobalConfigKey, func() interface{} {
		if f := ctx.Config().DexpreoptGlobalConfig(); f != "" {
			ctx.AddNinjaFileDeps(f)
			globalConfig, data, err := dexpreopt.LoadGlobalConfig(ctx, f)
			if err != nil {
				panic(err)
			}
			return globalConfigAndRaw{globalConfig, data}
		}

		// No global config filename set, see if there is a test config set
		return ctx.Config().Once(dexpreoptTestGlobalConfigKey, func() interface{} {
			// Nope, return a config with preopting disabled
			return globalConfigAndRaw{dexpreopt.GlobalConfig{
				DisablePreopt:          true,
				DisableGenerateProfile: true,
			}, nil}
		})
	}).(globalConfigAndRaw)
}

// setDexpreoptTestGlobalConfig sets a GlobalConfig that future calls to dexpreoptGlobalConfig will return.  It must
// be called before the first call to dexpreoptGlobalConfig for the config.
func setDexpreoptTestGlobalConfig(config android.Config, globalConfig dexpreopt.GlobalConfig) {
	config.Once(dexpreoptTestGlobalConfigKey, func() interface{} { return globalConfigAndRaw{globalConfig, nil} })
}

var dexpreoptGlobalConfigKey = android.NewOnceKey("DexpreoptGlobalConfig")
var dexpreoptTestGlobalConfigKey = android.NewOnceKey("TestDexpreoptGlobalConfig")

// Expected format for apexJarValue = <apex name>:<jar name>
func splitApexJarPair(apexJarValue string) (string, string)  {
	var apexJarPair []string = strings.SplitN(apexJarValue, ":", 2)
	if apexJarPair == nil || len(apexJarPair) != 2 {
		panic(fmt.Errorf("malformed apexJarValue: %q, expected format: <apex>:<jar>",
			apexJarValue))
	}
	return apexJarPair[0], apexJarPair[1]
}

// systemServerClasspath returns the on-device locations of the modules in the system server classpath.  It is computed
// once the first time it is called for any ctx.Config(), and returns the same slice for all future calls with the same
// ctx.Config().
func systemServerClasspath(ctx android.PathContext) []string {
	return ctx.Config().OnceStringSlice(systemServerClasspathKey, func() []string {
		global := dexpreoptGlobalConfig(ctx)

		var systemServerClasspathLocations []string
		for _, m := range global.SystemServerJars {
			systemServerClasspathLocations = append(systemServerClasspathLocations,
				filepath.Join("/system/framework", m+".jar"))
		}
		for _, m := range global.UpdatableSystemServerJars {
			apex, jar := splitApexJarPair(m)
			systemServerClasspathLocations = append(systemServerClasspathLocations,
				filepath.Join("/apex", apex, "javalib", jar + ".jar"))
		}
		return systemServerClasspathLocations
	})
}

var systemServerClasspathKey = android.NewOnceKey("systemServerClasspath")

// dexpreoptTargets returns the list of targets that are relevant to dexpreopting, which excludes architectures
// supported through native bridge.
func dexpreoptTargets(ctx android.PathContext) []android.Target {
	var targets []android.Target
	for _, target := range ctx.Config().Targets[android.Android] {
		if target.NativeBridge == android.NativeBridgeDisabled {
			targets = append(targets, target)
		}
	}

	return targets
}

func stemOf(moduleName string) string {
	// b/139391334: the stem of framework-minus-apex is framework
	// This is hard coded here until we find a good way to query the stem
	// of a module before any other mutators are run
	if moduleName == "framework-minus-apex" {
		return "framework"
	}
	return moduleName
}

// Construct a variant of the global config for dexpreopted bootclasspath jars. The variants differ
// in the list of input jars (libcore, framework, or both), in the naming scheme for the dexpreopt
// files (ART recognizes "apex" names as special), and whether to include a zip archive.
//
// 'name' is a string unique for each profile (used in directory names and ninja rule names)
// 'stem' is the basename of the image: the resulting filenames are <stem>[-<jar>].{art,oat,vdex}.
func getBootImageConfig(ctx android.PathContext, key android.OnceKey, name string, stem string,
	needZip bool, artApexJarsOnly bool) bootImageConfig {

	return ctx.Config().Once(key, func() interface{} {
		global := dexpreoptGlobalConfig(ctx)

		artModules := global.ArtApexJars
		imageModules := artModules

		var bootLocations []string

		for _, m := range artModules {
			bootLocations = append(bootLocations,
				filepath.Join("/apex/com.android.art/javalib", stemOf(m)+".jar"))
		}

		if !artApexJarsOnly {
			nonFrameworkModules := concat(artModules, global.ProductUpdatableBootModules)
			frameworkModules := android.RemoveListFromList(global.BootJars, nonFrameworkModules)
			imageModules = concat(imageModules, frameworkModules)

			for _, m := range frameworkModules {
				bootLocations = append(bootLocations,
					filepath.Join("/system/framework", stemOf(m)+".jar"))
			}
		}

		// The path to bootclasspath dex files needs to be known at module GenerateAndroidBuildAction time, before
		// the bootclasspath modules have been compiled.  Set up known paths for them, the singleton rules will copy
		// them there.
		// TODO(b/143682396): use module dependencies instead
		var bootDexPaths android.WritablePaths
		for _, m := range imageModules {
			bootDexPaths = append(bootDexPaths,
				android.PathForOutput(ctx, ctx.Config().DeviceName(), "dex_"+name+"jars_input", m+".jar"))
		}

		dir := android.PathForOutput(ctx, ctx.Config().DeviceName(), "dex_"+name+"jars")
		symbolsDir := android.PathForOutput(ctx, ctx.Config().DeviceName(), "dex_"+name+"jars_unstripped")

		var zip android.WritablePath
		if needZip {
			zip = dir.Join(ctx, stem+".zip")
		}

		targets := dexpreoptTargets(ctx)

		imageConfig := bootImageConfig{
			name:         name,
			stem:         stem,
			modules:      imageModules,
			dexLocations: bootLocations,
			dexPaths:     bootDexPaths,
			dir:          dir,
			symbolsDir:   symbolsDir,
			targets:      targets,
			images:       make(map[android.ArchType]android.OutputPath),
			imagesDeps:   make(map[android.ArchType]android.Paths),
			zip:          zip,
		}

		for _, target := range targets {
			imageDir := dir.Join(ctx, "system/framework", target.Arch.ArchType.String())
			imageConfig.images[target.Arch.ArchType] = imageDir.Join(ctx, stem+".art")

			imagesDeps := make([]android.Path, 0, len(imageConfig.modules)*3)
			for _, dep := range imageConfig.moduleFiles(ctx, imageDir, ".art", ".oat", ".vdex") {
				imagesDeps = append(imagesDeps, dep)
			}
			imageConfig.imagesDeps[target.Arch.ArchType] = imagesDeps
		}

		return imageConfig
	}).(bootImageConfig)
}

// Default config is the one that goes in the system image. It includes both libcore and framework.
var defaultBootImageConfigKey = android.NewOnceKey("defaultBootImageConfig")

func defaultBootImageConfig(ctx android.PathContext) bootImageConfig {
	return getBootImageConfig(ctx, defaultBootImageConfigKey, "boot", "boot", true, false)
}

// Apex config is used for the JIT-zygote experiment. It includes both libcore and framework, but AOT-compiles only libcore.
var apexBootImageConfigKey = android.NewOnceKey("apexBootImageConfig")

func apexBootImageConfig(ctx android.PathContext) bootImageConfig {
	return getBootImageConfig(ctx, apexBootImageConfigKey, "apex", "apex", false, false)
}

// ART config is the one used for the ART apex. It includes only libcore.
var artBootImageConfigKey = android.NewOnceKey("artBootImageConfig")

func artBootImageConfig(ctx android.PathContext) bootImageConfig {
	return getBootImageConfig(ctx, artBootImageConfigKey, "art", "boot", false, true)
}

func defaultBootclasspath(ctx android.PathContext) []string {
	return ctx.Config().OnceStringSlice(defaultBootclasspathKey, func() []string {
		global := dexpreoptGlobalConfig(ctx)
		image := defaultBootImageConfig(ctx)
		bootclasspath := append(copyOf(image.dexLocations), global.ProductUpdatableBootLocations...)
		return bootclasspath
	})
}

var defaultBootclasspathKey = android.NewOnceKey("defaultBootclasspath")

var copyOf = android.CopyOf

func init() {
	android.RegisterMakeVarsProvider(pctx, dexpreoptConfigMakevars)
}

func dexpreoptConfigMakevars(ctx android.MakeVarsContext) {
	ctx.Strict("PRODUCT_BOOTCLASSPATH", strings.Join(defaultBootclasspath(ctx), ":"))
	ctx.Strict("PRODUCT_DEX2OAT_BOOTCLASSPATH", strings.Join(defaultBootImageConfig(ctx).dexLocations, ":"))
	ctx.Strict("PRODUCT_SYSTEM_SERVER_CLASSPATH", strings.Join(systemServerClasspath(ctx), ":"))

	ctx.Strict("DEXPREOPT_BOOT_JARS_MODULES", strings.Join(defaultBootImageConfig(ctx).modules, ":"))
}
