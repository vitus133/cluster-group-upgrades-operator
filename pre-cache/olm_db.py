import sqlite3
import sys
import argparse


def parse_args():
    parser = argparse.ArgumentParser(
        description='Extract list of images from operator index database.')
    parser.add_argument('db_path', nargs='?', type=argparse.FileType('r'),
                        help="SQLite database path")
    parser.add_argument('packages_path', nargs='?',
                        type=argparse.FileType('r'),
                        help="Path to the list of packages file, \
                            where each line contains \
                            <package>:<channel> record")
    parser.add_argument('img_list_file', nargs='?',
                        type=argparse.FileType('a'),
                        help="Path to the image list file (appended).")

    args = parser.parse_args()
    if len(sys.argv) < 3:
        parser.print_help()
        exit(1)
    return args


def extract_bundle_names(args):
    with open(args.packages_path.name, args.packages_path.mode) as p:
        records = [i.split(":") for i in p.read().splitlines() if len(i) > 0]

    con = sqlite3.connect(args.db_path.name)
    cur = con.cursor()
    bundles = []
    for record in records:
        pkg = record[0].strip()
        channel = record[1].strip()
        query = (
            'SELECT head_operatorbundle_name FROM channel WHERE '
            f'package_name = "{pkg}" AND name = "{channel}"')
        result_set = cur.execute(query)
        result = result_set.fetchone()
        bundles.append(result[0])
    con.close()
    return bundles


def extract_images(args, bundles):
    con = sqlite3.connect(args.db_path.name)
    cur = con.cursor()
    bundle_str = ",".join([f'"{i}"' for i in bundles])
    query = f'SELECT image FROM related_image WHERE \
            operatorbundle_name IN ({bundle_str})'
    result_set = cur.execute(query)
    results = result_set.fetchall()
    con.close()
    return [item[0] for item in results]


if __name__ == "__main__":
    args = parse_args()

    bundles = extract_bundle_names(args)
    images = extract_images(args, bundles)

    if args.img_list_file is not None:
        with open(args.img_list_file.name, args.img_list_file.mode) as f:
            f.write('\n'.join(images))
    else:
        print('\n'.join(images))
