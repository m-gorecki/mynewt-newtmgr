/**
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements.  See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership.  The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License.  You may obtain a copy of the License at
 *
 *  http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package syscfg

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	log "github.com/Sirupsen/logrus"
	"github.com/spf13/cast"

	"mynewt.apache.org/newt/newt/interfaces"
	"mynewt.apache.org/newt/newt/newtutil"
	"mynewt.apache.org/newt/newt/pkg"
	"mynewt.apache.org/newt/util"
)

const SYSCFG_INCLUDE_SUBDIR = "include/syscfg"
const SYSCFG_HEADER_FILENAME = "syscfg.h"

const SYSCFG_PREFIX_API = "MYNEWT_API_"
const SYSCFG_PREFIX_PKG = "MYNEWT_PKG_"
const SYSCFG_PREFIX_SETTING = "MYNEWT_VAL_"

type CfgSettingType int

const (
	CFG_SETTING_TYPE_RAW CfgSettingType = iota
	CFG_SETTING_TYPE_TASK_PRIO
	CFG_SETTING_TYPE_INTERRUPT_PRIO
)

const SYSCFG_PRIO_ANY = "any"

// Reserve last 16 priorities for the system (sanity, idle).
const SYSCFG_TASK_PRIO_MAX = 0xef

// The range of interrupt priorities is hardware dependent, so don't limit
// these here.
const SYSCFG_INTERRUPT_PRIO_MAX = 0xffffffff

var cfgSettingNameTypeMap = map[string]CfgSettingType{
	"raw":                CFG_SETTING_TYPE_RAW,
	"task_priority":      CFG_SETTING_TYPE_TASK_PRIO,
	"interrupt_priority": CFG_SETTING_TYPE_INTERRUPT_PRIO,
}

type CfgPoint struct {
	Value  string
	Source *pkg.LocalPackage
}

type CfgEntry struct {
	Name        string
	Value       string
	History     []CfgPoint
	Description string
	SettingType CfgSettingType
}

type CfgRoster struct {
	settings    map[string]string
	pkgsPresent map[string]bool
	apisPresent map[string]bool
}

type Cfg struct {
	Settings    map[string]CfgEntry
	Roster      CfgRoster
	Orphans     map[string][]CfgPoint
	Ambiguities []CfgEntry
}

func newRoster() CfgRoster {
	return CfgRoster{
		settings:    map[string]string{},
		pkgsPresent: map[string]bool{},
		apisPresent: map[string]bool{},
	}
}

func NewCfg() Cfg {
	return Cfg{
		Settings: map[string]CfgEntry{},
		Roster:   newRoster(),
		Orphans:  map[string][]CfgPoint{},
	}
}

func WritePreamble(w io.Writer) {
	fmt.Fprintf(w, "/**\n * This file was generated by %s\n */\n\n",
		newtutil.NewtVersionStr)
}

func ValueIsTrue(val string) bool {
	if val == "" {
		return false
	}

	i, err := util.AtoiNoOct(val)
	if err == nil && i == 0 {
		return false
	}

	return true
}

func (cfg *Cfg) Features() map[string]bool {
	features := map[string]bool{}
	for k, v := range cfg.Settings {
		if v.IsTrue() {
			features[k] = true
		}
	}

	return features
}

func (cfg *Cfg) FeaturesForLpkg(lpkg *pkg.LocalPackage) map[string]bool {
	features := cfg.Features()

	for k, v := range lpkg.InjectedSettings() {
		_, ok := features[k]
		if ok {
			log.Warnf("Attempt to override syscfg setting %s with "+
				"injected feature from package %s", k, lpkg.Name())
		} else {
			if ValueIsTrue(v) {
				features[k] = true
			}
		}
	}

	return features
}

func (point CfgPoint) Name() string {
	if point.Source == nil {
		return "newt"
	} else {
		return point.Source.Name()
	}
}

func (entry *CfgEntry) IsTrue() bool {
	return ValueIsTrue(entry.Value)
}

func (entry *CfgEntry) appendValue(lpkg *pkg.LocalPackage, value interface{}) {
	strval := stringValue(value)
	point := CfgPoint{Value: strval, Source: lpkg}
	entry.History = append(entry.History, point)
	entry.Value = strval
}

func (entry *CfgEntry) ambiguousCount() int {
	diffVals := false
	count := 0
	for i := 1; i < len(entry.History)-1; i++ {
		cur := entry.History[len(entry.History)-i-1]
		next := entry.History[len(entry.History)-i]

		// If either setting is injected, there is no ambiguity
		if cur.Source == nil || next.Source == nil {
			break
		}

		// If the two package have different priorities, there is no ambiguity.
		if normalizePkgType(cur.Source.Type()) !=
			normalizePkgType(next.Source.Type()) {

			break
		}

		if cur.Value != next.Value {
			diffVals = true
		}

		count++
	}

	// Account for final package that was skipped in loop.
	if count > 0 {
		count++
	}

	// If all values are identical, there is no ambiguity
	if !diffVals {
		count = 0
	}

	return count
}

func (entry *CfgEntry) ambiguousText() string {
	count := entry.ambiguousCount()
	if count == 0 {
		return ""
	}

	str := fmt.Sprintf("%s [", entry.Name)

	for i := 0; i < count; i++ {
		cur := entry.History[len(entry.History)-i-1]
		if i != 0 {
			str += ", "
		}
		str += fmt.Sprintf("%s:%s", cur.Name(), cur.Value)
	}
	str += "]"

	return str
}

func FeatureToCflag(featureName string) string {
	return fmt.Sprintf("-D%s=1", settingName(featureName))
}

func stringValue(val interface{}) string {
	return strings.TrimSpace(cast.ToString(val))
}

func readSetting(name string, lpkg *pkg.LocalPackage,
	vals map[interface{}]interface{}) (CfgEntry, error) {

	entry := CfgEntry{}

	entry.Name = name
	entry.Description = stringValue(vals["description"])
	entry.Value = stringValue(vals["value"])
	if vals["type"] == nil {
		entry.SettingType = CFG_SETTING_TYPE_RAW
	} else {
		var ok bool
		typename := stringValue(vals["type"])
		entry.SettingType, ok = cfgSettingNameTypeMap[typename]
		if !ok {
			return entry, util.FmtNewtError(
				"setting %s specifies invalid type: %s", name, typename)
		}
	}

	entry.appendValue(lpkg, entry.Value)

	return entry, nil
}

func (cfg *Cfg) readDefsOnce(lpkg *pkg.LocalPackage,
	features map[string]bool) error {
	v := lpkg.Viper

	lfeatures := cfg.FeaturesForLpkg(lpkg)
	for k, _ := range features {
		lfeatures[k] = true
	}

	settings := newtutil.GetStringMapFeatures(v, lfeatures, "pkg.syscfg_defs")
	if settings != nil {
		for k, v := range settings {
			vals := v.(map[interface{}]interface{})
			entry, err := readSetting(k, lpkg, vals)
			if err != nil {
				return util.FmtNewtError("Config for package %s: %s",
					lpkg.Name(), err.Error())
			}

			if _, exists := cfg.Settings[k]; exists {
				// XXX: Better error message.
				return util.FmtNewtError("setting %s redefined", k)
			}
			cfg.Settings[k] = entry
		}
	}

	return nil
}

func (cfg *Cfg) readValsOnce(lpkg *pkg.LocalPackage,
	features map[string]bool) error {
	v := lpkg.Viper

	lfeatures := cfg.FeaturesForLpkg(lpkg)
	for k, _ := range features {
		lfeatures[k] = true
	}
	values := newtutil.GetStringMapFeatures(v, lfeatures, "pkg.syscfg_vals")
	if values != nil {
		for k, v := range values {
			entry, ok := cfg.Settings[k]
			if ok {
				entry.appendValue(lpkg, v)
				cfg.Settings[k] = entry
			} else {
				orphan := CfgPoint{
					Value:  stringValue(v),
					Source: lpkg,
				}
				cfg.Orphans[k] = append(cfg.Orphans[k], orphan)
			}

		}
	}

	return nil
}

func (cfg *Cfg) Log() {
	keys := make([]string, len(cfg.Settings))
	i := 0
	for k, _ := range cfg.Settings {
		keys[i] = k
		i++
	}
	sort.Strings(keys)

	log.Debugf("syscfg settings (%d entries):", len(cfg.Settings))
	for _, k := range keys {
		entry := cfg.Settings[k]

		str := fmt.Sprintf("    %s=%s [", k, entry.Value)

		for i, p := range entry.History {
			if i != 0 {
				str += ", "
			}
			str += fmt.Sprintf("%s:%s", p.Name(), p.Value)
		}
		str += "]"

		log.Debug(str)
	}

	keys = make([]string, len(cfg.Orphans))
	i = 0
	for k, _ := range cfg.Orphans {
		keys[i] = k
		i++
	}
	sort.Strings(keys)

	for _, k := range keys {
		str := fmt.Sprintf("ignoring override of undefined setting %s [", k)
		for i, p := range cfg.Orphans[k] {
			if i != 0 {
				str += ", "
			}
			str += fmt.Sprintf("%s:%s", p.Name(), p.Value)
		}
		str += "]"

		log.Warnf(str)
	}
}

func (cfg *Cfg) DetectErrors() error {
	if len(cfg.Ambiguities) == 0 {
		return nil
	}

	str := "Syscfg ambiguities detected:"
	for _, entry := range cfg.Ambiguities {
		str += "\n    " + entry.ambiguousText()
	}
	return util.NewNewtError(str)
}

func escapeStr(s string) string {
	s = strings.Replace(s, "/", "_", -1)
	s = strings.Replace(s, "-", "_", -1)
	s = strings.Replace(s, " ", "_", -1)
	s = strings.ToUpper(s)
	return s
}

func isSettingVal(s string) bool {
	return strings.HasPrefix(s, SYSCFG_PREFIX_SETTING)
}

func isPkgVal(s string) bool {
	return strings.HasPrefix(s, SYSCFG_PREFIX_PKG)
}

func isApiVal(s string) bool {
	return strings.HasPrefix(s, SYSCFG_PREFIX_API)
}

func settingName(setting string) string {
	return SYSCFG_PREFIX_SETTING + escapeStr(setting)
}

func pkgPresentName(pkgName string) string {
	return SYSCFG_PREFIX_PKG + escapeStr(pkgName)
}

func apiPresentName(apiName string) string {
	return SYSCFG_PREFIX_API + strings.ToUpper(apiName)
}

func normalizePkgType(typ interfaces.PackageType) interfaces.PackageType {
	switch typ {
	case pkg.PACKAGE_TYPE_TARGET:
		return pkg.PACKAGE_TYPE_TARGET
	case pkg.PACKAGE_TYPE_APP:
		return pkg.PACKAGE_TYPE_APP
	case pkg.PACKAGE_TYPE_UNITTEST:
		return pkg.PACKAGE_TYPE_UNITTEST
	case pkg.PACKAGE_TYPE_BSP:
		return pkg.PACKAGE_TYPE_BSP
	default:
		return pkg.PACKAGE_TYPE_LIB
	}
}

func categorizePkgs(lpkgs []*pkg.LocalPackage) map[interfaces.PackageType][]*pkg.LocalPackage {
	pmap := map[interfaces.PackageType][]*pkg.LocalPackage{
		pkg.PACKAGE_TYPE_TARGET:   []*pkg.LocalPackage{},
		pkg.PACKAGE_TYPE_APP:      []*pkg.LocalPackage{},
		pkg.PACKAGE_TYPE_UNITTEST: []*pkg.LocalPackage{},
		pkg.PACKAGE_TYPE_BSP:      []*pkg.LocalPackage{},
		pkg.PACKAGE_TYPE_LIB:      []*pkg.LocalPackage{},
	}

	for _, lpkg := range lpkgs {
		typ := normalizePkgType(lpkg.Type())
		pmap[typ] = append(pmap[typ], lpkg)
	}

	for k, v := range pmap {
		pmap[k] = pkg.SortLclPkgs(v)
	}

	return pmap
}

func (cfg *Cfg) readForPkgType(lpkgs []*pkg.LocalPackage,
	features map[string]bool) error {

	for _, lpkg := range lpkgs {
		if err := cfg.readDefsOnce(lpkg, features); err != nil {
			return err
		}
	}
	for _, lpkg := range lpkgs {
		if err := cfg.readValsOnce(lpkg, features); err != nil {
			return err
		}
	}

	return nil
}

func detectAmbiguities(cfg Cfg) Cfg {
	for _, entry := range cfg.Settings {
		if entry.ambiguousCount() > 0 {
			cfg.Ambiguities = append(cfg.Ambiguities, entry)
		}
	}

	return cfg
}

func Read(lpkgs []*pkg.LocalPackage, apis []string,
	injectedSettings map[string]string, features map[string]bool) (Cfg, error) {

	cfg := NewCfg()
	for k, v := range injectedSettings {
		cfg.Settings[k] = CfgEntry{
			Name:        k,
			Description: "Injected setting",
			Value:       v,
			History: []CfgPoint{{
				Value:  v,
				Source: nil,
			}},
		}

		if ValueIsTrue(v) {
			features[k] = true
		}
	}

	// Read system configuration files.  In case of conflicting settings, the
	// higher priority pacakge's setting wins.  Package priorities are assigned
	// as follows (highest priority first):
	//     * target
	//     * app (if present)
	//     * unittest (if no app)
	//     * bsp
	//     * everything else (lib, sdk, compiler)

	lpkgMap := categorizePkgs(lpkgs)

	for _, ptype := range []interfaces.PackageType{
		pkg.PACKAGE_TYPE_LIB,
		pkg.PACKAGE_TYPE_BSP,
		pkg.PACKAGE_TYPE_UNITTEST,
		pkg.PACKAGE_TYPE_APP,
		pkg.PACKAGE_TYPE_TARGET,
	} {
		if err := cfg.readForPkgType(lpkgMap[ptype], features); err != nil {
			return cfg, err
		}
	}

	cfg.buildCfgRoster(lpkgs, apis)
	if err := fixupSettings(cfg); err != nil {
		return cfg, err
	}

	cfg = detectAmbiguities(cfg)

	return cfg, nil
}

func mostRecentPoint(entry CfgEntry) CfgPoint {
	if len(entry.History) == 0 {
		panic("invalid cfg entry; len(history) == 0")
	}

	return entry.History[len(entry.History)-1]
}

func calcPriorities(cfg Cfg, settingType CfgSettingType, max int,
	allowDups bool) error {

	// setting-name => entry
	anyEntries := map[string]CfgEntry{}

	// priority-value => entry
	valEntries := map[int]CfgEntry{}

	for name, entry := range cfg.Settings {
		if entry.SettingType == settingType {
			if entry.Value == SYSCFG_PRIO_ANY {
				anyEntries[name] = entry
			} else {
				prio, err := util.AtoiNoOct(entry.Value)
				if err != nil {
					return util.FmtNewtError(
						"invalid priority value: setting=%s value=%s pkg=%s",
						name, entry.Value, entry.History[0].Name())
				}

				if prio > max {
					return util.FmtNewtError(
						"invalid priority value: value too great (> %d); "+
							"setting=%s value=%s pkg=%s",
						max, entry.Name, entry.Value,
						mostRecentPoint(entry).Name())
				}

				if !allowDups {
					if oldEntry, ok := valEntries[prio]; ok {
						return util.FmtNewtError(
							"duplicate priority value: setting1=%s "+
								"setting2=%s pkg1=%s pkg2=%s value=%s",
							oldEntry.Name, entry.Name, entry.Value,
							oldEntry.History[0].Name(),
							entry.History[0].Name())
					}
				}

				valEntries[prio] = entry
			}
		}
	}

	greatest := 0
	for prio, _ := range valEntries {
		if prio > greatest {
			greatest = prio
		}
	}

	anyNames := make([]string, 0, len(anyEntries))
	for name, _ := range anyEntries {
		anyNames = append(anyNames, name)
	}
	sort.Strings(anyNames)

	for _, name := range anyNames {
		entry := anyEntries[name]

		greatest++
		if greatest > max {
			return util.FmtNewtError("could not assign 'any' priority: "+
				"value too great (> %d); setting=%s value=%s pkg=%s",
				max, name, greatest,
				mostRecentPoint(entry).Name())
		}

		entry.Value = strconv.Itoa(greatest)
		cfg.Settings[name] = entry
	}

	return nil
}

func writeCheckMacros(w io.Writer) {
	s := `/**
 * These macros exists to ensure code includes this header when needed.  If
 * code checks the existence of a setting directly via ifdef without including
 * this header, the setting macro will silently evaluate to 0.  In contrast, an
 * attempt to use these macros without including this header will result in a
 * compiler error.
 */
#define MYNEWT_VAL(x)                           MYNEWT_VAL_ ## x
#define MYNEWT_PKG(x)                           MYNEWT_PKG_ ## x
#define MYNEWT_API(x)                           MYNEWT_API_ ## x
`
	fmt.Fprintf(w, "%s\n", s)
}

func writeComment(entry CfgEntry, w io.Writer) {
	if len(entry.History) > 1 {
		fmt.Fprintf(w, "/* Overridden by %s (defined by %s) */\n",
			mostRecentPoint(entry).Name(),
			entry.History[0].Name())
	}
}

func writeDefine(key string, value string, w io.Writer) {
	fmt.Fprintf(w, "#ifndef %s\n", key)
	fmt.Fprintf(w, "#define %s (%s)\n", key, value)
	fmt.Fprintf(w, "#endif\n")
}

func (cfg *Cfg) specialValues() (apis, pkgs, settings []string) {
	for _, entry := range cfg.Settings {
		if isApiVal(entry.Value) {
			apis = append(apis, entry.Value)
		} else if isPkgVal(entry.Value) {
			pkgs = append(pkgs, entry.Value)
		} else if isSettingVal(entry.Value) {
			settings = append(settings, entry.Value)
		}
	}

	return
}

func (cfg *Cfg) buildCfgRoster(lpkgs []*pkg.LocalPackage, apis []string) {
	roster := CfgRoster{
		settings:    make(map[string]string, len(cfg.Settings)),
		pkgsPresent: make(map[string]bool, len(lpkgs)),
		apisPresent: make(map[string]bool, len(apis)),
	}

	for k, v := range cfg.Settings {
		roster.settings[settingName(k)] = v.Value
	}

	for _, v := range lpkgs {
		roster.pkgsPresent[pkgPresentName(v.Name())] = true
	}

	for _, v := range apis {
		roster.apisPresent[apiPresentName(v)] = true
	}

	apisNotPresent, pkgsNotPresent, _ := cfg.specialValues()

	for _, v := range apisNotPresent {
		_, ok := roster.apisPresent[v]
		if !ok {
			roster.apisPresent[v] = false
		}
	}

	for _, v := range pkgsNotPresent {
		_, ok := roster.pkgsPresent[v]
		if !ok {
			roster.pkgsPresent[v] = false
		}
	}

	cfg.Roster = roster
}

func settingValueToConstant(value string,
	roster CfgRoster) (string, bool, error) {

	seen := map[string]struct{}{}
	curVal := value
	for {
		v, ok := roster.settings[curVal]
		if ok {
			if _, ok := seen[v]; ok {
				return "", false, util.FmtNewtError("Syscfg cycle detected: "+
					"%s <==> %s", value, v)
			}
			seen[v] = struct{}{}
			curVal = v
		} else {
			break
		}
	}
	if curVal != value {
		return curVal, true, nil
	}

	v, ok := roster.apisPresent[value]
	if ok {
		if v {
			return "1", true, nil
		} else {
			return "0", true, nil
		}
	}

	v, ok = roster.pkgsPresent[value]
	if ok {
		if v {
			return "1", true, nil
		} else {
			return "0", true, nil
		}
	}

	return value, false, nil
}

func fixupSettings(cfg Cfg) error {
	for k, entry := range cfg.Settings {
		value, changed, err := settingValueToConstant(entry.Value, cfg.Roster)
		if err != nil {
			return err
		}

		if changed {
			entry.Value = value
			cfg.Settings[k] = entry
		}
	}

	return nil
}

func UnfixedValue(entry CfgEntry) string {
	point := mostRecentPoint(entry)
	return point.Value
}

func EntriesByPkg(cfg Cfg) map[string][]CfgEntry {
	pkgEntries := map[string][]CfgEntry{}
	for _, v := range cfg.Settings {
		name := v.History[0].Name()
		pkgEntries[name] = append(pkgEntries[name], v)
	}
	return pkgEntries
}

func writeSettingsOnePkg(cfg Cfg, pkgName string, pkgEntries []CfgEntry,
	w io.Writer) {

	fmt.Fprintf(w, "/*** %s */\n", pkgName)

	names := make([]string, len(pkgEntries), len(pkgEntries))
	for i, entry := range pkgEntries {
		names[i] = entry.Name
	}
	sort.Strings(names)

	first := true
	for _, n := range names {
		entry := cfg.Settings[n]
		if entry.Value != "" {
			if first {
				first = false
			} else {
				fmt.Fprintf(w, "\n")
			}

			writeComment(entry, w)
			writeDefine(settingName(n), entry.Value, w)
		}
	}
}

func writeSettings(cfg Cfg, w io.Writer) {
	// Group settings by package name so that the generated header file is
	// easier to read.
	pkgEntries := EntriesByPkg(cfg)

	pkgNames := make([]string, 0, len(pkgEntries))
	for name, _ := range pkgEntries {
		pkgNames = append(pkgNames, name)
	}
	sort.Strings(pkgNames)

	fmt.Fprintf(w, "/***** Settings */\n")

	for _, name := range pkgNames {
		fmt.Fprintf(w, "\n")
		entries := pkgEntries[name]
		writeSettingsOnePkg(cfg, name, entries, w)
	}
}

func writePkgsPresent(roster CfgRoster, w io.Writer) {
	present := make([]string, 0, len(roster.pkgsPresent))
	notPresent := make([]string, 0, len(roster.pkgsPresent))
	for k, v := range roster.pkgsPresent {
		if v {
			present = append(present, k)
		} else {
			notPresent = append(notPresent, k)
		}
	}

	sort.Strings(present)
	sort.Strings(notPresent)

	fmt.Fprintf(w, "/*** Packages (present) */\n")
	for _, symbol := range present {
		fmt.Fprintf(w, "\n")
		writeDefine(symbol, "1", w)
	}

	fmt.Fprintf(w, "\n")
	fmt.Fprintf(w, "/*** Packages (not present)*/\n")
	for _, symbol := range notPresent {
		fmt.Fprintf(w, "\n")
		writeDefine(symbol, "0", w)
	}
}

func writeApisPresent(roster CfgRoster, w io.Writer) {
	present := make([]string, 0, len(roster.apisPresent))
	notPresent := make([]string, 0, len(roster.apisPresent))
	for k, v := range roster.apisPresent {
		if v {
			present = append(present, k)
		} else {
			notPresent = append(notPresent, k)
		}
	}

	sort.Strings(present)
	sort.Strings(notPresent)

	fmt.Fprintf(w, "/*** APIs (present) */\n")
	for _, symbol := range present {
		fmt.Fprintf(w, "\n")
		writeDefine(symbol, "1", w)
	}

	fmt.Fprintf(w, "\n")
	fmt.Fprintf(w, "/*** APIs (not present) */\n")
	for _, symbol := range notPresent {
		writeDefine(symbol, "0", w)
		fmt.Fprintf(w, "\n")
	}
}

func write(cfg Cfg, w io.Writer) {
	WritePreamble(w)

	fmt.Fprintf(w, "#ifndef H_MYNEWT_SYSCFG_\n")
	fmt.Fprintf(w, "#define H_MYNEWT_SYSCFG_\n\n")

	writeCheckMacros(w)
	fmt.Fprintf(w, "\n")

	writeSettings(cfg, w)
	fmt.Fprintf(w, "\n")

	writePkgsPresent(cfg.Roster, w)
	fmt.Fprintf(w, "\n")

	writeApisPresent(cfg.Roster, w)
	fmt.Fprintf(w, "\n")

	fmt.Fprintf(w, "#endif\n")
}

func writeRequired(contents []byte, path string) (bool, error) {
	oldHeader, err := ioutil.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// File doesn't exist; write required.
			return true, nil
		}

		return true, util.NewNewtError(err.Error())
	}

	rc := bytes.Compare(oldHeader, contents)
	return rc != 0, nil
}

func headerPath(targetPath string) string {
	return fmt.Sprintf("%s/%s/%s", targetPath, SYSCFG_INCLUDE_SUBDIR,
		SYSCFG_HEADER_FILENAME)
}

func EnsureWritten(cfg Cfg, lpkgs []*pkg.LocalPackage,
	apis []string, targetPath string) error {

	if err := calcPriorities(cfg, CFG_SETTING_TYPE_TASK_PRIO,
		SYSCFG_TASK_PRIO_MAX, false); err != nil {

		return err
	}

	if err := calcPriorities(cfg, CFG_SETTING_TYPE_INTERRUPT_PRIO,
		SYSCFG_INTERRUPT_PRIO_MAX, true); err != nil {

		return err
	}

	cfg.buildCfgRoster(lpkgs, apis)
	if err := fixupSettings(cfg); err != nil {
		return err
	}

	buf := bytes.Buffer{}
	write(cfg, &buf)

	path := headerPath(targetPath)

	writeReqd, err := writeRequired(buf.Bytes(), path)
	if err != nil {
		return err
	}
	if !writeReqd {
		log.Debugf("syscfg unchanged; not writing header file (%s).", path)
		return nil
	}

	log.Debugf("syscfg changed; writing header file (%s).", path)

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return util.NewNewtError(err.Error())
	}

	if err := ioutil.WriteFile(path, buf.Bytes(), 0644); err != nil {
		return util.NewNewtError(err.Error())
	}

	return nil
}
