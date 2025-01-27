@TestOn('windows')
import 'dart:io';
import 'package:dart_either/dart_either.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:ubuntupro/core/agent_api_paths.dart';

void main() {
  tearDownAll(() => File('./.address').deleteSync());

  test('read port from line', () {
    const port = 56768;
    const line = '[::]:$port';

    // Exercises the parsing algorithm.
    final res = readAgentPortFromLine(line);

    expect(res, port);
  });

  test('line parsing error', () {
    const port = 56768;
    const line = '[::]-$port';

    // Exercises the parsing algorithm.
    final res = readAgentPortFromLine(line);

    expect(res, isNull);
  });

  test('read port from addr file', () async {
    const filePath = './.address';
    const port = 56768;
    const line = '[::]:$port';
    final addr = File(filePath);
    addr.writeAsStringSync(line);

    // Exercises the expected usage: reading from a file
    final res = await readAgentPortFromFile(filePath);

    expect(res.orNull(), port);
  });

  test('invalid file name', () async {
    const filePath = '\\<>';

    // Exercises the expected usage: reading from a file
    final res = await readAgentPortFromFile(filePath);

    expect(res, const Left(AgentAddrFileError.nonexistent));
  });

  test('empty file', () async {
    const filePath = './.address';
    final addr = File(filePath);
    addr.writeAsStringSync('');

    // Exercises the expected usage: reading from a file
    final res = await readAgentPortFromFile(filePath);

    expect(res, const Left(AgentAddrFileError.isEmpty));
  });

  test('access denied', () async {
    const filePath = './.address';
    final addr = File(filePath);
    addr.writeAsStringSync('');

    await IOOverrides.runZoned(
      () async {
        // Exercises the expected usage: reading from a file
        final res = await readAgentPortFromFile(filePath);

        expect(res, const Left(AgentAddrFileError.accessDenied));
      },
      createFile: (_) => throw const FileSystemException('access denied'),
    );
  });

  test('bad format', () async {
    const filePath = './.address';
    const port = 56768;
    const line = 'Hello World $port';
    final addr = File(filePath);
    addr.writeAsStringSync(line);

    // Exercises the expected usage: reading from a file
    final res = await readAgentPortFromFile(filePath);

    expect(res, const Left(AgentAddrFileError.formatError));
  });
}
