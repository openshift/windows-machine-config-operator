#!/bin/bash

if [ "$#" -lt 1 ]; then
	echo "Error: incorrect parameter count $#" >&2
	exit 1
fi

# Pass in source file with govc creds/settings e.g. sourcedevqe
source $1

# Specify the starting directory for the search
search_dir="."

# Find all files with extension .ovf
for file in $(find $serch_dir -type f -name "*.ovf"); do
	sed -i "s/vmware.cdrom.iso/vmware.cdrom.remotepassthrough/g" $file
done

# Specify the output file to store the line-by-line content
output_file="ovf_files_content.txt"

# Find all *.ovf files in the specified directory and its subdirectories
find "$search_dir" -type f -name "*.ovf" -print0 | while IFS= read -r -d '' file; do
	# For each found file, list its contents line by line into the output file
	echo "$file" > "$output_file"
	done
echo "OVF file contents saved to $output_file"

# Import each host one by one
for server in $(cat $output_file); do
	echo $server
	govc import.ovf -ds=vsanDatastore -folder=windows-golden-images $server
done
