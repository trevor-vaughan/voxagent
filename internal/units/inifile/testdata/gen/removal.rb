# frozen_string_literal: true

# Generates the removal corpus (spec 8.1, 4): remove_section, which 6.6.x added
# and ini_section reaches. 6.6.0 and 6.6.1 only: 6.5.0 has none. remove_setting's
# remove_empty_section: true is not ported: no provider passes it.
#
#   RUBYLIB=<release>/lib ruby removal.rb > removal-<release>.json     (see gen.sh)
require_relative 'edits_harness'

abort 'removal.rb: this release has no remove_empty_section' unless REMOVE_SECTION_KW

BASIC = "[a]\nx = 1\ny = 2\n\n[b]\nz = 3\n\n[c]\nw = 4\n"


# remove_section
add('rsec-middle', BASIC, [['remove_section', 'b'], ['section_names'], ['section?', 'b'], ['save']])
add('rsec-first', BASIC, [['remove_section', 'a'], ['section_names'], ['save']])
add('rsec-last', BASIC, [['remove_section', 'c'], ['section_names'], ['save']])
add('rsec-missing', BASIC, [['remove_section', 'nope'], ['section_names'], ['save']])
add('rsec-missing-file', :missing, [['remove_section', 'a'], ['save']])
add('rsec-then-update-later-section', BASIC, [['remove_section', 'a'], ['set_value', 'c', 'w', ' = ', %w[7 8]], ['set_value', 'b', 'z', ' = ', ['9']], ['save']])
add('rsec-then-create-same-name', BASIC, [['remove_section', 'b'], ['set_value', 'b', 'k', ' = ', ['v']], ['save']])
add('rsec-twice', BASIC, [['remove_section', 'b'], ['remove_section', 'b'], ['save']])
add('rsec-two-sections', BASIC, [['remove_section', 'a'], ['remove_section', 'c'], ['section_names'], ['save']])
add('rsec-all-sections', "[a]\nx = 1\n[b]\ny = 1\n", [['remove_section', 'a'], ['remove_section', 'b'], ['section_names'], ['save']])
add('rsec-global-with-content', "g = 1\n[a]\nx = 1\n", [['remove_section', ''], ['section_names'], ['save']])
add('rsec-global-empty', "[a]\nx = 1\n", [['remove_section', ''], ['section_names'], ['save']])
add('rsec-global-empty-file', '', [['remove_section', '']])
add('rsec-section-only-header', "[a]\n[b]\ny = 1\n", [['remove_section', 'a'], ['save']])
add('rsec-no-final-newline', "[a]\nx = 1\n[b]\ny = 1", [['remove_section', 'b'], ['save']])
add('rsec-new-section-in-this-run', BASIC, [['set_value', 'n'], ['remove_section', 'n'], ['section_names'], ['save']])
add('rsec-new-section-with-setting-in-this-run', BASIC, [['set_value', 'n', 'k', ' = ', ['v']], ['remove_section', 'n'], ['section_names'], ['save']])
add('rsec-dup-headers', "[a]\nx = 1\n[a]\ny = 2\n[b]\nz = 1\n", [['remove_section', 'a'], ['section_names'], ['save']])
add('rsec-dup-headers-then-update', "[a]\nx = 1\n[a]\ny = 2\n[b]\nz = 1\n", [['remove_section', 'a'], ['set_value', 'b', 'z', ' = ', %w[1 2]], ['save']])
add('rsec-empty-name-header', "[]\nx = 1\n[a]\ny = 2\n", [['remove_section', ''], ['section_names'], ['save']])
add('rsec-custom-syntax', "<a>\nx = 1\n<b>\ny = 1\n", [['remove_section', 'a'], ['section_names'], ['save']], 'prefix' => '<', 'suffix' => '>')
add('rsec-then-reread', BASIC, [['remove_section', 'b'], ['save'], ['reopen'], ['section_names'], ['get_value', 'c', 'w']])
add('rsec-invalid-bytes-fails-at-open', "[a]\nx = \xff\n".b, [['remove_section', 'a']])

# around an empty section
add('rsec-before-empty-section', "[a]\nx = 1\n[e]\n[x]\nk = 1\n", [['remove_section', 'a'], ['set_value', 'x', 'k', ' = ', ['9']], ['save']])
add('rsec-empty-section', "[a]\nx = 1\n[e]\n[x]\nk = 1\n", [['remove_section', 'e'], ['set_value', 'x', 'k', ' = ', ['9']], ['save']])
add('rsec-empty-section-at-eof', "[a]\nx = 1\n[e]\n", [['remove_section', 'e'], ['save']])

emit
