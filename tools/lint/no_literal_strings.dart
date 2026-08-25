// §3.6: "No hardcoded user-facing strings. .arb files, flutter gen-l10n.
// CI fails on a literal string in a widget file (lint rule)."
//
// Arabic is a launch requirement, not a later phase. The failure mode this
// guards against is not a missing translation — it is a string that was never
// translatable in the first place, discovered three weeks before launch when
// somebody switches locale and half the UI is still English.
//
//   dart run tools/lint/no_literal_strings.dart app/lib
//
// Heuristic by necessity: a full analyzer pass would be exact but needs the
// analyzer package, which this repo cannot depend on (see ADR-001's amendment).
// The heuristic is tuned to be *loud rather than clever* — a false positive is
// silenced with an explicit ignore comment, which leaves a reviewable record.

import 'dart:io';

/// Widget constructors whose string arguments end up on screen.
const _userFacingWidgets = <String>{
  'Text', 'SelectableText', 'RichText', 'TextSpan',
  'AppBar', 'SnackBar', 'AlertDialog', 'SimpleDialog',
  'Tooltip', 'Semantics', 'ListTile', 'Chip',
  'ElevatedButton', 'TextButton', 'OutlinedButton', 'FilledButton',
};

/// Named parameters that render text.
const _userFacingParams = <String>{
  'label', 'labelText', 'hintText', 'helperText', 'errorText',
  'title', 'subtitle', 'message', 'tooltip', 'semanticLabel',
  'placeholder', 'counterText', 'prefixText', 'suffixText',
};

/// A literal that cannot be user-facing text, whatever the context.
bool _isBenign(String value) {
  final v = value.trim();
  if (v.isEmpty) return true;
  if (v.length == 1) return true; // separators like ' ', '/', '·'
  // Identifiers, routes, keys, asset paths, MIME types, format strings.
  if (RegExp(r'^[a-z0-9_]+$').hasMatch(v)) return true;
  if (RegExp(r'^[a-z0-9_.\-/]+$').hasMatch(v)) return true;
  if (v.startsWith('/') || v.startsWith('assets/') || v.startsWith('packages/')) return true;
  if (v.startsWith('http://') || v.startsWith('https://') || v.startsWith('wss://')) return true;
  // No letters at all: symbols, punctuation, digits.
  if (!RegExp(r'[A-Za-z؀-ۿ]').hasMatch(v)) return true;
  return false;
}

void main(List<String> args) {
  final root = Directory(args.isEmpty ? 'lib' : args.first);
  if (!root.existsSync()) {
    stderr.writeln('no_literal_strings: ${root.path} does not exist');
    exit(2);
  }

  final violations = <String>[];
  var filesScanned = 0;

  final files = root
      .listSync(recursive: true)
      .whereType<File>()
      .where((f) => f.path.endsWith('.dart'))
      // Generated code is not ours to lint.
      .where((f) => !f.path.endsWith('.g.dart'))
      .where((f) => !f.path.endsWith('.freezed.dart'))
      .where((f) => !f.path.contains('l10n'))
      .where((f) => !f.path.contains('generated'));

  for (final file in files) {
    filesScanned++;
    final lines = file.readAsLinesSync();

    for (var i = 0; i < lines.length; i++) {
      final line = lines[i];
      final trimmed = line.trimLeft();

      if (trimmed.startsWith('//') || trimmed.startsWith('*') || trimmed.startsWith('/*')) {
        continue;
      }
      // An explicit, reviewable escape hatch.
      if (line.contains('// ignore: no_literal_strings')) continue;
      if (i > 0 && lines[i - 1].contains('// ignore_for_file: no_literal_strings')) continue;
      if (lines.take(20).any((l) => l.contains('// ignore_for_file: no_literal_strings'))) break;

      final touchesUserFacing =
          _userFacingWidgets.any((w) => line.contains('$w(')) ||
          _userFacingParams.any((p) => line.contains('$p:'));
      if (!touchesUserFacing) continue;

      for (final match in RegExp(r"""(['"])((?:\\.|(?!\1).)*)\1""").allMatches(line)) {
        final value = match.group(2) ?? '';
        if (_isBenign(value)) continue;

        violations.add(
          '${file.path}:${i + 1}\n'
          '    $trimmed\n'
          "    -> literal '$value' is user-facing. Move it to an .arb file and\n"
          '       reference it via context.l10n, or add "// ignore: no_literal_strings"\n'
          '       with a reason if it genuinely is not shown to a user.',
        );
      }
    }
  }

  stdout.writeln('no_literal_strings: scanned $filesScanned file(s)');

  if (violations.isNotEmpty) {
    stdout.writeln('\n${violations.length} hardcoded user-facing string(s):\n');
    for (final v in violations) {
      stdout.writeln(v);
      stdout.writeln();
    }
    stdout.writeln('§3.6 makes Arabic a launch requirement. A string that was never');
    stdout.writeln('translatable cannot be translated later by a translator.');
    exit(1);
  }

  stdout.writeln('no_literal_strings: OK');
}
