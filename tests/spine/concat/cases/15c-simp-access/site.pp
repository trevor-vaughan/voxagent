concat_file { '/etc/security/access.conf':
  tag => '_etc_security_access.conf',
  owner => 'root',
  group => 'root',
  mode => '0644',
  replace => true,
  backup => 'puppet',
  show_diff => true,
  order => 'alpha',
  ensure_newline => true,
  format => 'plain',
  force => false,
  create_empty_file => true,
}
concat_fragment { '/etc/security/access.conf_header':
  target => '/etc/security/access.conf',
  tag => '_etc_security_access.conf',
  content => '# This file is managed by Puppet. DO NOT EDIT.
',
  order => '0',
}
concat_fragment { 'pam_access_rule_default_deny':
  target => '/etc/security/access.conf',
  tag => '_etc_security_access.conf',
  order => 9999999999,
  content => '-:ALL:ALL
',
}
concat_fragment { 'pam_access_rule_allow_local_root':
  target => '/etc/security/access.conf',
  tag => '_etc_security_access.conf',
  order => 1,
  content => '+:root:LOCAL
',
}
concat_fragment { 'pam_access_rule_Allow administrators':
  target => '/etc/security/access.conf',
  tag => '_etc_security_access.conf',
  order => 1000,
  content => '# Allow the administrators to access the system from anywhere
+:(administrators):ALL
',
}
concat_fragment { 'pam_access_rule_Allow security':
  target => '/etc/security/access.conf',
  tag => '_etc_security_access.conf',
  order => 1000,
  content => '# Allow the security to access the system from anywhere
+:(security):10.0.0.0/8
',
}
concat_fragment { 'pam_access_rule_cron_user_root':
  target => '/etc/security/access.conf',
  tag => '_etc_security_access.conf',
  order => 1000,
  content => '+:root:cron,crond
',
}
