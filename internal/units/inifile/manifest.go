package inifile

import "github.com/voxagent/voxagent/internal/unit"

// Each type is fingerprinted by its own files (spec §2). The three Ruby
// libraries every type loads come first in the shared list.
var (
	sharedFiles     = []string{"puppet/util/ini_file.rb", "puppet/util/ini_file/section.rb", "puppet/util/external_iterator.rb"}
	settingFiles    = append([]string{"puppet/type/ini_setting.rb", "puppet/provider/ini_setting/ruby.rb"}, sharedFiles...)
	subsettingFiles = append([]string{"puppet/type/ini_subsetting.rb", "puppet/provider/ini_subsetting/ruby.rb", "puppet/util/setting_value.rb"}, sharedFiles...)
	sectionFiles    = append([]string{"puppet/type/ini_section.rb", "puppet/provider/ini_section/ruby.rb"}, sharedFiles...)
)

// Manifest describes the inifile unit: puppetlabs-inifile 6.5.0 (the release
// SIMP pins), 6.6.0 and 6.6.1. ini_section exists from 6.6.0 on. Platforms is
// any: the providers only edit files. The ini_section digests are those of the
// shipped, unpatched files (spec §2). It requires group for
// group.ForeignProvider.
var Manifest = unit.Manifest{
	Name:      "inifile",
	Version:   "1.0.0",
	Source:    unit.Origin{Implementation: "puppetlabs-inifile", Version: ""},
	Oracle:    unit.Origin{Implementation: "openvox", Version: "8.34.0"},
	Types:     []string{"ini_setting", "ini_subsetting", "ini_section"},
	Requires:  []string{"group"},
	Platforms: unit.AnyPlatform,
	Releases: []unit.Release{{
		Version: "6.5.0",
		Files: []unit.FileDigest{
			{Path: "puppet/util/external_iterator.rb", SHA256: "f5fe4626636cf5f0e6ec83f9a38b0a292a888cd8e1ce82d1e05c43638698db73"},
			{Path: "puppet/util/ini_file.rb", SHA256: "af5c5c805adb3de7c7b8a783eed9c772bbc80f563ea826fe21ed6dea9e855068"},
			{Path: "puppet/util/ini_file/section.rb", SHA256: "af071d708bc10151fcfb8d9c5457e998f89c922295871dc5974084d6a5563e4b"},
			{Path: "puppet/util/setting_value.rb", SHA256: "265a397a95f75812da5a7271fea969eaf4a167eeeb7d57e529842616fb97cf94"},
			{Path: "puppet/type/ini_setting.rb", SHA256: "73811669f1f4f2fe9ab4aadf1b23c25a015413b7fa16039b07ec455d312e9185"},
			{Path: "puppet/provider/ini_setting/ruby.rb", SHA256: "8f2ef19ad6b5adc67504df8e6e9fedd04d793e455193652696a85c42990c880b"},
			{Path: "puppet/type/ini_subsetting.rb", SHA256: "38140bb3d9ff50f6c6fe3672c8b428e55cd0ae3e73039d64b9998417aea8a95c"},
			{Path: "puppet/provider/ini_subsetting/ruby.rb", SHA256: "071acf4804f9e50ba617c221f17dac5e5fb55de389d14bb5b18cb2378c753bc0"},
		},
		Types: map[string][]string{"ini_setting": settingFiles, "ini_subsetting": subsettingFiles},
	}, {
		Version: "6.6.0",
		Files: []unit.FileDigest{
			{Path: "puppet/util/external_iterator.rb", SHA256: "f5fe4626636cf5f0e6ec83f9a38b0a292a888cd8e1ce82d1e05c43638698db73"},
			{Path: "puppet/util/ini_file.rb", SHA256: "939b8d770aa61ec55b519ec697be3d9f74d00125c2c464cf89a1ccd207693d66"},
			{Path: "puppet/util/ini_file/section.rb", SHA256: "af9ba52ee72c033f94139a2e3f5b5677a3773b2bb1f094d14c9b35c4b6890bed"},
			{Path: "puppet/util/setting_value.rb", SHA256: "265a397a95f75812da5a7271fea969eaf4a167eeeb7d57e529842616fb97cf94"},
			{Path: "puppet/type/ini_setting.rb", SHA256: "73811669f1f4f2fe9ab4aadf1b23c25a015413b7fa16039b07ec455d312e9185"},
			{Path: "puppet/provider/ini_setting/ruby.rb", SHA256: "275c993745468f9a276c966f860353c324fa7c3279249aa67ddfbd68c9cd22fd"},
			{Path: "puppet/type/ini_subsetting.rb", SHA256: "38140bb3d9ff50f6c6fe3672c8b428e55cd0ae3e73039d64b9998417aea8a95c"},
			{Path: "puppet/provider/ini_subsetting/ruby.rb", SHA256: "ba1765d7b7fedb54571b41a823bdd14bbdbbeab17c0e8611c1664460deade90b"},
			{Path: "puppet/type/ini_section.rb", SHA256: "52f3feaf64adcab905adfc2675822e7953429f83afcca56d138c1a5215847d36"},
			{Path: "puppet/provider/ini_section/ruby.rb", SHA256: "a844a97fbe055d2bd98af7305175d8842a8737a72c5ef95541f8b4ae4b14666f"},
		},
		Types: map[string][]string{"ini_setting": settingFiles, "ini_subsetting": subsettingFiles, "ini_section": sectionFiles},
	}, {
		Version: "6.6.1",
		Files: []unit.FileDigest{
			{Path: "puppet/util/external_iterator.rb", SHA256: "f5fe4626636cf5f0e6ec83f9a38b0a292a888cd8e1ce82d1e05c43638698db73"},
			{Path: "puppet/util/ini_file.rb", SHA256: "70e6dc6e1bb8823b2a78a82f87efb2b9c7c9b88862385eac9f7fe42066087599"},
			{Path: "puppet/util/ini_file/section.rb", SHA256: "af9ba52ee72c033f94139a2e3f5b5677a3773b2bb1f094d14c9b35c4b6890bed"},
			{Path: "puppet/util/setting_value.rb", SHA256: "265a397a95f75812da5a7271fea969eaf4a167eeeb7d57e529842616fb97cf94"},
			{Path: "puppet/type/ini_setting.rb", SHA256: "73811669f1f4f2fe9ab4aadf1b23c25a015413b7fa16039b07ec455d312e9185"},
			{Path: "puppet/provider/ini_setting/ruby.rb", SHA256: "cde0831b86c4060b061cbb42252322d372ab0a4fb1d32c382111f67987342e64"},
			{Path: "puppet/type/ini_subsetting.rb", SHA256: "38140bb3d9ff50f6c6fe3672c8b428e55cd0ae3e73039d64b9998417aea8a95c"},
			{Path: "puppet/provider/ini_subsetting/ruby.rb", SHA256: "ba1765d7b7fedb54571b41a823bdd14bbdbbeab17c0e8611c1664460deade90b"},
			{Path: "puppet/type/ini_section.rb", SHA256: "52f3feaf64adcab905adfc2675822e7953429f83afcca56d138c1a5215847d36"},
			{Path: "puppet/provider/ini_section/ruby.rb", SHA256: "b0844bc5b5dffd0cee0c1bfaba597d14b046ebcd6d1714fc21a497d92fdbfdd0"},
		},
		Types: map[string][]string{"ini_setting": settingFiles, "ini_subsetting": subsettingFiles, "ini_section": sectionFiles},
	}},
}
