# Wowzers (VIBECODED)

A free music streaming site that uses the OpenDeezer API.

<img width="603" height="1311" alt="IMG_6321" src="https://github.com/user-attachments/assets/e7930404-07ec-4e44-bb27-b2d98317faeb" />
<img width="603" height="1311" alt="IMG_6322" src="https://github.com/user-attachments/assets/3ea189e1-8f08-4011-acc9-d16f72985325" />

# Set up deployment (RENDER)

**Grabbing the ARL**
Sign up for https://render.com and add a new web service with this git repository as the option. You can select a Free instance. **DO NOT DEPLOY YET.**  

In a desktop browser, sign up for a Deezer account. Once logged in, Open DevTools (F12 or right-click --> Inspect) and click on the Storage tab. Expand Cookies, then click "https://www.deezer.com".   Copy your ARL.  

Set an environment variable, with the NAME_OF_VARIABLE displaying DEEZER_ARL and the value displaying the ARL you copied from DevTools. Deploy the service.  

You should be able to use the website and playback should work! If the site is stuck on the deployment page, refresh it.  



**Credits**

OpenDeezer by Cycl0o0: https://github.com/Cycl0o0/OpenDeezer
