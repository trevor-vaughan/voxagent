concat_file { '/etc/c10ok.conf':   validate_cmd => '/usr/bin/test -s %' }
concat_file { '/etc/c10fail.conf': validate_cmd => '/usr/bin/false %' }
concat_file { '/etc/c10failexist.conf': validate_cmd => '/usr/bin/false %' }
concat_file { '/etc/c10nopct.conf': validate_cmd => '/usr/bin/true' }
concat_file { '/etc/c10stderr.conf': validate_cmd => '/bin/sh -c "echo oops-out; echo oops-err >&2; exit 3" -- %' }
['ok','fail','failexist','nopct','stderr'].each |$x| {
  concat_fragment { "f-${x}": target => "/etc/c10${x}.conf", content => "${x}\n" }
}
