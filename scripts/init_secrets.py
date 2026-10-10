"""One-shot offline Compose helper. Copies host-owned 0600 secrets for UID 10001."""
import os
import pathlib
import shutil
root=pathlib.Path('/state/credentials')
root.mkdir(mode=0o700,exist_ok=True)
for name in ['tokens.json','signing.key']:
    temp=root/(name+'.tmp')
    with open('/run/secrets/'+name,'rb') as src, open(temp,'wb') as dest:
        shutil.copyfileobj(src,dest)
    os.chmod(temp,0o600);os.chown(temp,10001,10001)
    temp.replace(root/name)
os.chmod(root,0o700);os.chown(root,10001,10001)
