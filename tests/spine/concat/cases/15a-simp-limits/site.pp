concat_file { '/etc/security/limits.conf':
  tag => '_etc_security_limits.conf',
  owner => 'root',
  group => 'root',
  mode => '0640',
  replace => true,
  backup => 'puppet',
  show_diff => true,
  order => 'numeric',
  ensure_newline => true,
  format => 'plain',
  force => false,
  create_empty_file => true,
}
concat_fragment { '/etc/security/limits.conf_header':
  target => '/etc/security/limits.conf',
  tag => '_etc_security_limits.conf',
  content => '# This file is managed by Puppet. DO NOT EDIT.
',
  order => '0',
}
concat_fragment { 'pam_limits_rule_max_logins':
  target => '/etc/security/limits.conf',
  tag => '_etc_security_limits.conf',
  order => 100,
  content => '*	hard	maxlogins	10
',
}
concat_fragment { 'pam_limits_rule_prevent_core':
  target => '/etc/security/limits.conf',
  tag => '_etc_security_limits.conf',
  order => 100,
  content => '*	hard	core	0
',
}
