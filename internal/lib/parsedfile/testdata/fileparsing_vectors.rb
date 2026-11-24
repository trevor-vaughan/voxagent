# frozen_string_literal: true

# Generates the "want" of every case in vectors.json by running the real
# Puppet::Util::FileParsing (fileparsing.rb), and prints the whole file.
#
#   ruby fileparsing_vectors.rb vectors.json > vectors.json.new
#
# Cases carry name, grammar, op, their inputs and (after this runs) want.
# Encodings: a value is null (nil), {"absent":true} (:absent), {"s":"x"}
# (String) or {"list":[...]} (Array; an element is a String or
# {"absent":true}); a record is {"type","line","fields"}.
#
# The grammars below are the generic ones of the engine's tests, cron's
# (record definitions copied from cron_core 1.3.0
# lib/puppet/provider/cron/crontab.rb:9-98) and host's (host_core
# lib/puppet/provider/host/parsed.rb:21-48). internal/lib/parsedfile's
# vectors_test.go defines the generic ones again in Go and
# TestRubyMatchesSharedVectors re-runs this script and compares.
# String#inspect escapes non-ASCII unless the external encoding is UTF-8;
# pin it so the wants do not depend on the locale of the machine that runs this.
Encoding.default_external = Encoding::UTF_8

require 'json'
require 'puppet'
require 'puppet/util/fileparsing'

TIME_FIELDS = %i[minute hour monthday month weekday].freeze

GRAMMARS = {
  # Whitespace split, rollup, optional fields, rts true (/\s+$/).
  'split' => lambda { |g|
    g.text_line :comment, match: /^#/
    g.record_line :r, fields: %w[a b c], optional: %w[b c], separator: /\s+/, rts: true
  },
  # Split on ":" (a one-character separator).
  'colon' => lambda { |g|
    g.record_line :c, fields: %w[x y], separator: /:/
  },
  # Tab separator and joiner.
  'tabjoin' => lambda { |g|
    g.record_line :t, fields: %w[a b c], optional: %w[b c], separator: /\t/, joiner: "\t"
  },
  # Match mode with an optional group: a non-participating group is nil.
  'match' => lambda { |g|
    g.record_line :m, fields: %w[x y z], match: /^(\w+) ?(\w+)?$/
  },
  # Match mode with absent "*": a capture equal to it is :absent.
  'matchabs' => lambda { |g|
    g.record_line :m, fields: %w[x y], match: /^(\S*) (\S*)$/, absent: '*', optional: %w[x y]
  },
  # Match mode with the default absent "": an empty capture is :absent.
  'matchempty' => lambda { |g|
    g.record_line :m, fields: %w[x y], match: /^(\S*) (\S*)$/, optional: %w[x y]
  },
  # rts /^\s+/ strips leading whitespace of the joined line.
  'lead' => lambda { |g|
    g.record_line :l, fields: %w[a b], optional: %w[a], separator: /\s+/, rts: /^\s+/
  },
  # First matching text type wins.
  'order' => lambda { |g|
    g.text_line :c1, match: /a/
    g.text_line :c2, match: /a/
    g.text_line :c3, match: /b/
  },
  # pre_gen runs on a copy and may change the line.
  'pregen' => lambda { |g|
    g.text_line :t, match: /./, pre_gen: proc { |r| r[:line] = "X#{r[:line]}" }
  },
  # post_parse adds a field; to_line replaces join and rts.
  'hook' => lambda { |g|
    g.record_line :h, fields: %w[a b], separator: /\s+/, rts: true,
                      post_parse: proc { |r| r[:seen] = 'yes' },
                      to_line: proc { |r| "<#{r[:a]}|#{r[:b]}>  " }
  },
  # host_core lib/puppet/provider/host/parsed.rb:21-48, including its
  # :incomplete pattern, which Onigmo decides.
  'host' => lambda { |g|
    g.text_line :comment, match: %r{^#}
    g.text_line :blank, match: %r{^\s*$}
    hosts_pattern = '^([0-9a-f:]\S+)\s+([^#\s+]\S+)\s*(.*?)?(?:\s*#\s*(.*))?$'
    g.record_line :parsed, fields: ['ip', 'name', 'host_aliases', 'comment'],
                           optional: ['host_aliases', 'comment'],
                           match: %r{#{hosts_pattern}},
                           post_parse: proc { |hash|
                                         # An absent comment should match "comment => ''"
                                         hash[:comment] = '' if hash[:comment].nil? || hash[:comment] == :absent
                                         unless hash[:host_aliases].nil? || hash[:host_aliases] == :absent
                                           hash[:host_aliases].gsub!(%r{\s+}, ' ') # Change delimiter
                                         end
                                       },
                           to_line: proc { |hash|
                                      [:ip, :name].each do |n|
                                        raise ArgumentError, _('%{attr} is a required attribute for hosts') % { attr: n } unless hash[n] && hash[n] != :absent
                                      end
                                      str = "#{hash[:ip]}\t#{hash[:name]}"
                                      if hash.include?(:host_aliases) && !hash[:host_aliases].nil? && hash[:host_aliases] != :absent
                                        str += "\t#{hash[:host_aliases]}"
                                      end
                                      if hash.include?(:comment) && !hash[:comment].empty?
                                        str += "\t# #{hash[:comment]}"
                                      end
                                      str
                                    }

    g.text_line :incomplete, match: %r{(?! (#{hosts_pattern}))}
  },
  # cron_core 1.3.0 crontab.rb:9-98.
  'cron' => lambda { |g|
    g.text_line :comment, match: %r{^\s*#}, post_parse: proc { |record|
      record[:name] = Regexp.last_match(1) if record[:line] =~ %r{Puppet Name: (.+)\s*$}
    }

    g.text_line :blank, match: %r{^\s*$}

    g.text_line :environment, match: %r{^\s*\w+\s*=}

    g.record_line :crontab,
                  fields: ['time', 'command'],
                  match: %r{^\s*(@\w+|\S+\s+\S+\s+\S+\s+\S+\s+\S+)\s+(.+)$},
                  absent: '*',
                  block_eval: :instance do
      def post_parse(record)
        time = record.delete(:time)
        match = %r{@(\S+)}.match(time)
        if match
          TIME_FIELDS.each { |f| record[f] = :absent }
          record[:special] = match.captures[0]
          return record
        end

        match = %r{(\S+)\s+(\S+)\s+(\S+)\s+(\S+)\s+(\S+)}.match(time)
        if match
          record[:special] = :absent
          TIME_FIELDS.zip(match.captures).each do |field, value|
            record[field] = if value == absent
                              :absent
                            else
                              value.split(',')
                            end
          end

          return record
        end

        raise Puppet::Error, _('Line got parsed as a crontab entry but cannot be handled. Please file a bug with the contents of your crontab')
      end

      def pre_gen(record)
        if record[:special] && record[:special] != :absent
          record[:special] = "@#{record[:special]}"
        end

        TIME_FIELDS.each do |field|
          if (vals = record[field]) && vals.is_a?(Array)
            record[field] = vals.join(',')
          end
        end
        record
      end

      def to_line(record)
        str = ''
        record[:name] = nil if record[:unmanaged]
        str = "# Puppet Name: #{record[:name]}\n" if record[:name]
        if record[:environment] && record[:environment] != :absent
          str += record[:environment].map { |line| "#{line}\n" }.join('')
        end
        fields = if record[:special] && record[:special] != :absent
                   [:special, :command]
                 else
                   TIME_FIELDS + [:command]
                 end
        str += record.values_at(*fields).map { |field|
          if field.nil? || field == :absent
            absent
          else
            field
          end
        }.join(joiner)
        str
      end
    end
  }
}.freeze

def grammar(name)
  g = Object.new
  g.extend(Puppet::Util::FileParsing)
  GRAMMARS.fetch(name).call(g)
  g
end

def decode_value(v)
  return nil if v.nil?
  return :absent if v['absent']
  return v['s'] if v.key?('s')

  v['list'].map { |e| e.is_a?(Hash) ? :absent : e }
end

def encode_value(v)
  case v
  when nil then nil
  when :absent then { 'absent' => true }
  when String then { 's' => v }
  when Array then { 'list' => v.map { |e| e == :absent ? { 'absent' => true } : e } }
  else raise "unencodable #{v.inspect}"
  end
end

def decode_record(r)
  h = { record_type: r['type'].to_sym }
  h[:line] = r['line'] if r.key?('line')
  (r['fields'] || {}).each { |k, v| h[k.to_sym] = decode_value(v) }
  h
end

def encode_record(h)
  fields = h.reject { |k, _| %i[record_type line].include?(k) }
  { 'type' => h[:record_type].to_s, 'line' => h.fetch(:line, ''),
    'fields' => fields.to_h { |k, v| [k.to_s, encode_value(v)] } }
end

def run(c)
  g = grammar(c['grammar'])
  case c['op']
  when 'lines' then { 'lines' => g.lines(c['text']) }
  when 'parse' then { 'records' => g.parse(c['text']).map { |h| encode_record(h) } }
  when 'to_line' then { 'line' => g.to_line(decode_record(c['record'])) }
  when 'to_file' then { 'text' => g.to_file(c['records'].map { |r| decode_record(r) }) }
  else raise "unknown op #{c['op']}"
  end
rescue Puppet::ResourceError => e
  { 'error' => e.to_s }
rescue ArgumentError, Puppet::Error => e
  { 'error' => e.message }
end

vectors = JSON.parse(File.read(ARGV[0]))
vectors['cases'] = vectors['cases'].map { |c| c.merge('want' => run(c)) }
puts JSON.pretty_generate(vectors)
