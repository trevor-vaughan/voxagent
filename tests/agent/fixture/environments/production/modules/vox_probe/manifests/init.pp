class vox_probe {
  notify { 'vox_probe says hello': }

  notify { "fact=${facts['vox_fact']}": }

  file { '/tmp/vox_probe_content':
    ensure  => file,
    content => "inline content from the catalog\n",
    mode    => '0644',
  }

  file { '/tmp/vox_probe_source':
    ensure => file,
    source => 'puppet:///modules/vox_probe/x',
    mode   => '0644',
  }

  file { '/tmp/vox_probe_tree':
    ensure  => directory,
    recurse => true,
    source  => 'puppet:///modules/vox_probe/tree',
  }

  vox_marker { '/tmp/vox_probe_marker':
    ensure => present,
  }
}
