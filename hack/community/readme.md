# Cut a new community release
The generate.sh script is called in the pre step to set up for the 
aws-e2e-operator-test. The script copies over the WMCO community 
bundle to the artifact directory while also replacing csv fields such as the 
name, description, and date. Once generated into the artifact directory, the 
bundle is used in the windows-e2e-operator test. After it passes, the bundle can
be used to release a new community WMCO.

### After the test passes
- Fork and clone [community-operators-prod](https://github.com/redhat-openshift-ecosystem/community-operators-prod)
repo.
- Copy the community bundle from the ARTIFACT_DIR.
- Paste the bundle contents into a new folder under 
community-windows-machine-config-operator. The folder title should be the
next version of the community WMCO that is to be released.
- Scan over the bundle CSV and annotations to ensure all the necessary fields
were replaced. Add or adjust fields as needed including olm.skipRange, name, 
version, and replaces-mode.

### Ask for reviews
- Tag WINC team for review
