import sys
import json
import argparse
import traceback

def parse_args():
    parser = argparse.ArgumentParser(
        description='Extract list of related images from operator index.')
    parser.add_argument('index_download_path', nargs='?',
                        help="Path where opm index is exportrd")
    parser.add_argument('operators_spec_file', nargs='?',
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


def extract_images(args, objects):
    bundles = []
    with open(args.operators_spec_file.name, 'r') as p:
        records = [i.split(":") for i in p.read().splitlines() if len(i) > 0]
    packages = []
    channels = {}
    for item in records:
        channels[item[0].strip()] = item[1].strip()
        packages.append(item[0].strip())
    for item in objects:
        if item.get("schema") == "olm.channel":
            if item.get("package") in packages and item.get("name") == channels[item.get("package")]:
                latest = item.get("entries")[-1].get("name")
                bundles.append(latest)
    images = []
    for item in objects:
        if item.get("schema") == "olm.bundle":
            if item.get("name") in bundles and item.get("package") in packages:
                images.extend([elem.get("image") for elem in item.get("relatedImages")])
    return images

def load_rendered_index():
    with open("/tmp/index.json", "r") as f:
        data = f.read().lstrip()
    # Rendered index is not a valid json, but a list
    # of concatenated json blocks. Hence the raw decoder and the loop
    decoder = json.JSONDecoder()
    objects = []
    while data:
        obj, index = decoder.raw_decode(data)
        objects.append(obj)
        data = data[index:].lstrip()
    return objects


if __name__ == "__main__":
    try:
        args = parse_args()
        data = load_rendered_index()
        images = extract_images(args, data)
        with open(args.img_list_file.name, args.img_list_file.mode) as f:
            f.write('\n'.join(images))
            f.write('\n')
        exit(0)
    except Exception as e:
        print(e)
        traceback.print_exc(file=sys.stdout)
        exit(1)
