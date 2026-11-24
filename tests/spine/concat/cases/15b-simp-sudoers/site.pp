concat_file { '/etc/sudoers':
  tag => '_etc_sudoers',
  owner => 'root',
  group => 'root',
  mode => '0440',
  replace => true,
  backup => 'puppet',
  show_diff => true,
  order => 'alpha',
  ensure_newline => false,
  validate_cmd => '/usr/sbin/visudo -q -c -f %',
  format => 'plain',
  force => false,
  create_empty_file => true,
}
concat_fragment { 'sudo_default_entry_00_main':
  target => '/etc/sudoers',
  tag => '_etc_sudoers',
  order => 80,
  content => 'Defaults    !visiblepw, always_set_home, match_group_by_gid, always_query_group_plugin, listpw=all, requiretty, syslog=authpriv, !root_sudo, !umask, secure_path = /usr/local/sbin:/usr/local/bin:/sbin:/bin:/usr/sbin:/usr/bin, env_reset, env_keep = "COLORS DISPLAY HOSTNAME HISTSIZE INPUTRC KDEDIR \\
      LS_COLORS MAIL PS1 PS2 QTDIR USERNAME \\
      LANG LC_ADDRESS LC_CTYPE LC_COLLATE LC_IDENTIFICATION \\
      LC_MEASUREMENT LC_MESSAGES LC_MONETARY LC_NAME LC_NUMERIC \\
      LC_PAPER LC_TELEPHONE LC_TIME LC_ALL LANGUAGE LINGUAS \\
      _XKB_CHARSET XAUTHORITY"
',
}
concat_fragment { 'sudo_user_specification_admin global':
  target => '/etc/sudoers',
  tag => '_etc_sudoers',
  order => 90,
  content => '%administrators    simp, simp.example.com=(root) ROLE=unconfined_r NOPASSWD:EXEC:SETENV: /bin/su - root
',
}
concat_fragment { 'sudo_user_specification_admin run puppet':
  target => '/etc/sudoers',
  tag => '_etc_sudoers',
  order => 90,
  content => '%administrators    simp, simp.example.com=(root) ROLE=unconfined_r NOPASSWD:EXEC:SETENV: /usr/sbin/puppet, /opt/puppetlabs/bin/puppet
',
}
concat_fragment { 'sudo_user_specification_admin clean puppet certs':
  target => '/etc/sudoers',
  tag => '_etc_sudoers',
  order => 90,
  content => '%administrators    simp, simp.example.com=(root) ROLE=unconfined_r NOPASSWD:EXEC:SETENV: /bin/rm -rf /etc/puppetlabs/puppet/ssl
',
}
concat_fragment { 'sudo_user_alias_admins':
  target => '/etc/sudoers',
  tag => '_etc_sudoers',
  order => 16,
  content => 'User_Alias ADMINS = administrators, wheel
',
}
concat_fragment { 'sudo_user_alias_auditors':
  target => '/etc/sudoers',
  tag => '_etc_sudoers',
  order => 16,
  content => 'User_Alias AUDITORS = security
',
}
