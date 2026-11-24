concat_file { '/etc/c09new.conf': owner => 'daemon', group => 'bin', mode => '0640' }
concat_file { '/etc/c09exist.conf': owner => 'daemon', group => 'bin', mode => '0600' }
concat_file { '/etc/c09sel.conf': seluser => 'system_u', selrole => 'object_r', seltype => 'etc_t', selrange => 's0' }
concat_file { '/etc/c09seldef.conf': selinux_ignore_defaults => true }
concat_file { '/etc/c09numid.conf': owner => 2, group => '1', mode => '640' }
['new','exist','sel','seldef','numid'].each |$x| {
  concat_fragment { "f-${x}": target => "/etc/c09${x}.conf", content => "${x}\n" }
}
