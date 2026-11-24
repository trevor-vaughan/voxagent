# Native concat beside a Tier 3 type: stdlib's file_line makes voxagent load
# the catalog into the extension host, so File[/etc/cx3.conf] comes from the
# host's generate pass (hostLoaded) and its content from native eval_generate.
concat_file { '/etc/cx3.conf': }
concat_fragment { 'fx3b': target => '/etc/cx3.conf', content => "two\n" }
concat_fragment { 'fx3a': target => '/etc/cx3.conf', content => "one\n" }
file_line { 'cx3-line': path => '/etc/cx3-other.conf', line => 'added' }
