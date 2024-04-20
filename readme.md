
Create a database, the database, host, user for the database and the password will
be needed for the config file. The database will be needed to cache geocode requests.

Example:
CREATE DATABASE sfsoap;
CREATE USER sfsoap@localhost IDENTIFIED BY 'test';
GRANT ALL PRIVILEGES ON sfsoap.* TO sfsoap@localhost;
FLUSH PRIVILEGES;

Optional, load the seeds/AddressGeoData.sql.gz into MySQL:
zcat seeds/AddressGeoData.sql.gz | mysql -u root sfsoap

Directory setup required:

/bin/mkdir ./log
/bin/mkdir ./persistentdata
/bin/mkdir -p filescache/account_logos  
/bin/mkdir -p filescache/account_textimages
/bin/chmod 750 ./persistentdata
/bin/chmod 750 filescache/account_logos
/bin/chmod 750 filescache/account_textimages
/bin/chmod 750 ./log

1. Download and install go (only if the command go version does not return a recent version)
    - see https://golang.org/doc/install
2. sfsoap
    - download this sfsoap directory
    - place this sfsoap directory one level higher than the projectfolder (webroot) of goodcompany
3. Build sfsoap (optional)
    - go to the sfsoap folder
    - $ go build .
4. Run sfsoap
    - go to the sfsoap folder, if you build a sfsoap executable, you can do ./sfsoap
    - Otherwise: $ go run .
      Startup params:  
      -cfgFilePath: Sets the location (path + filename) of the environment file. REQUIRED!

For the brand setting of the config file, we support:
* andwork
* jouwict
* goodcompany


**example of cfgFile file contents:**  
appmode: dev  
brand: andwork  
httpport: 8080
udspath: @/tmp/sfsoap
username: xxxx@jobsmediagroup.com.full  
password: somepassword  
securitytoken: somesecuritytoken  
  
consumerkey: someconsumerkey  
consumersecret: someconsumersecret  
  
endpointurirest: /services/data/v50.0/  
urlloginrest: https://test.salesforce.com/services/oauth2/token  

persistentdatadir: ./persistentdata
cachedir: ./filescache

**JWT LOGIN**
Make ssl certificates:

    openssl genpkey -des3 -algorithm RSA -pass pass:SomePassword -out server.pass.key -pkeyopt rsa_keygen_bits:2048
    openssl rsa -passin pass:SomePassword -in server.pass.key -out server.key
    openssl req -new -key server.key -out server.csr
    openssl x509 -req -sha256 -days 365 -in server.csr -signkey server.key -out server.crt

https://developer.salesforce.com/docs/atlas.en-us.sfdx_dev.meta/sfdx_dev/sfdx_dev_auth_key_and_cert.htm

 ---
 1. Go to setup>apps>app manager in salesforce. 
 2. Click edit on 'Api Access App'. 
 3. Enable OAuth and enable 'Use digital signatures'. 
 4. And upload the server.crt there.

---

 1. Go to 'Manage connected Apps' in setup.
 2. Click on our 'Api Access App' 
 3. Under profiles add the integration user (tech user). 
 4. Click on 'Edit policies' (on top)
 5. On new screen, set 'Admin users are pre authorized'

---

Visiting this is probably not necesarry then, but it might be, and you might need to login as intergation user:
visit: https://<your-instance>.salesforce.com/services/oauth2/authorize?response_type=token&client_id=<consumer-key>&redirect_uri=<redirect-url>

