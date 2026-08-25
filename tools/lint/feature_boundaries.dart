// §4.2: "A feature never imports another feature's data/ or presentation/.
// Cross-feature needs go through domain/ interfaces or a shared core/ contract.
// Enforce with an import lint rule in CI — this is the difference between a
// real modular codebase and a claim of one."
//
// And §4.1: "Dependencies point inward. The domain layer imports no Flutter,
// no Dio, no Drift."
//
//   dart run tools/lint/feature_boundaries.dart app/lib
//
// This file is the enforcement. Without it, §4.2 is a paragraph in a document
// and the architecture decays quietly over about a month — the first violation
// is always reasonable ("I just need one model from rooms"), and the twentieth
// is why the codebase cannot be reasoned about.

import 'dart:io';

class Violation {
  Violation(this.file, this.line, this.import, this.rule, this.fix);

  final String file;
  final int line;
  final String import;
  final String rule;
  final String fix;

  @override
  String toString() => '$file:$line\n'
      "    import '$import'\n"
      '    -> $rule\n'
      '       $fix';
}

void main(List<String> args) {
  final root = Directory(args.isEmpty ? 'lib' : args.first);
  if (!root.existsSync()) {
    stderr.writeln('feature_boundaries: ${root.path} does not exist');
    exit(2);
  }

  final violations = <Violation>[];
  var scanned = 0;

  final importRe = RegExp(r"""^\s*import\s+['"]([^'"]+)['"]""");
  // package:vybe/features/<name>/<layer>/...
  final featurePathRe = RegExp(r'features[/\\]([a-z0-9_]+)[/\\]([a-z0-9_]+)');

  for (final entity in root.listSync(recursive: true)) {
    if (entity is! File || !entity.path.endsWith('.dart')) continue;
    if (entity.path.endsWith('.g.dart') || entity.path.endsWith('.freezed.dart')) continue;

    scanned++;
    final normalised = entity.path.replaceAll(r'\', '/');
    final selfMatch = featurePathRe.firstMatch(normalised);
    final selfFeature = selfMatch?.group(1);
    final selfLayer = selfMatch?.group(2);
    final inDomainLayer = selfLayer == 'domain' || normalised.contains('/core/domain/');

    final lines = entity.readAsLinesSync();
    for (var i = 0; i < lines.length; i++) {
      final m = importRe.firstMatch(lines[i]);
      if (m == null) continue;
      final imported = m.group(1)!;

      // --- Rule 1: the domain layer imports no framework or I/O ----------
      if (inDomainLayer) {
        const forbidden = {
          'package:flutter/': 'Flutter',
          'package:dio/': 'Dio',
          'package:drift/': 'Drift',
          'package:flutter_riverpod/': 'Riverpod',
          'package:go_router/': 'go_router',
          'package:cached_network_image/': 'cached_network_image',
          'dart:io': 'dart:io',
          'dart:ui': 'dart:ui',
        };
        for (final entry in forbidden.entries) {
          if (imported.startsWith(entry.key) || imported == entry.key) {
            violations.add(Violation(
              normalised, i + 1, imported,
              'the domain layer must not import ${entry.value} (§4.1: dependencies point inward)',
              'Define an interface in domain/ and implement it in data/ or presentation/.',
            ));
          }
        }
      }

      // --- Rule 2: no reaching into another feature's internals ----------
      final importedMatch = featurePathRe.firstMatch(imported);
      if (importedMatch != null && selfFeature != null) {
        final otherFeature = importedMatch.group(1)!;
        final otherLayer = importedMatch.group(2)!;

        if (otherFeature != selfFeature &&
            (otherLayer == 'data' || otherLayer == 'presentation')) {
          violations.add(Violation(
            normalised, i + 1, imported,
            "feature '$selfFeature' reaches into feature '$otherFeature'"
                " $otherLayer/ (§4.2)",
            "Depend on features/$otherFeature/domain/ instead, or lift the shared "
                'contract into core/. If neither fits, the two features are one feature.',
          ));
        }
      }

      // --- Rule 3: core/ never depends on a feature ----------------------
      if (normalised.contains('/core/') && imported.contains('features/')) {
        violations.add(Violation(
          normalised, i + 1, imported,
          'core/ must not depend on any feature (§4.2: core is the shared floor)',
          'Invert it: define the contract in core/ and let the feature implement it.',
        ));
      }
    }
  }

  stdout.writeln('feature_boundaries: scanned $scanned file(s)');

  if (violations.isNotEmpty) {
    stdout.writeln('\n${violations.length} boundary violation(s):\n');
    for (final v in violations) {
      stdout.writeln(v);
      stdout.writeln();
    }
    stdout.writeln('§4.2: "an unenforced boundary is a comment."');
    exit(1);
  }

  stdout.writeln('feature_boundaries: OK');
}
